package redis

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/bsm/ratelimit.v1"
)

var (
	errClosed      = errors.New("redis: client is closed")
	errPoolTimeout = errors.New("redis: connection pool timeout")
)

// PoolStats contains pool state information and accumulated stats.
type PoolStats struct {
	Requests uint32 // number of times a connection was requested by the pool
	Hits     uint32 // number of times free connection was found in the pool
	Waits    uint32 // number of times the pool had to wait for a connection
	Timeouts uint32 // number of times a wait timeout occurred

	TotalConns uint32 // the number of total connections in the pool
	FreeConns  uint32 // the number of free connections in the pool
}

type pool interface {
	First() *conn
	Get() (*conn, bool, error)
	Put(*conn) error
	Remove(*conn, error) error
	Len() int
	FreeLen() int
	Close() error
	Stats() *PoolStats
}

// connStack is used as a LIFO to maintain free connection
type connStack struct {
	cns  []*conn
	free chan struct{}
	mx   sync.Mutex
}

func newConnStack(max int) *connStack {
	return &connStack{
		cns:  make([]*conn, 0, max),
		free: make(chan struct{}, max),
	}
}

func (s *connStack) Len() int { return len(s.free) }

func (s *connStack) Push(cn *conn) {
	s.mx.Lock()
	s.cns = append(s.cns, cn)
	s.mx.Unlock()
	s.free <- struct{}{}
}

func (s *connStack) Pop() *conn {
	select {
	case <-s.free:
		return s.pop()
	default:
		return nil
	}
}

func (s *connStack) PopWithTimeout(d time.Duration) *conn {
	select {
	case <-s.free:
		return s.pop()
	case <-time.After(d):
		return nil
	}
}

func (s *connStack) pop() (cn *conn) {
	s.mx.Lock()
	ci := len(s.cns) - 1
	cn, s.cns = s.cns[ci], s.cns[:ci]
	s.mx.Unlock()
	return
}

// connList stores all known connections, usable or not
type connList struct {
	cns  map[*conn]struct{}
	mx   sync.Mutex
	size int32 // atomic
	max  int32
}

func newConnList(max int) *connList {
	return &connList{
		cns: make(map[*conn]struct{}, max),
		max: int32(max),
	}
}

func (l *connList) Len() int {
	return int(atomic.LoadInt32(&l.size))
}

// Reserve reserves place in the list and returns true on success. The
// caller must add or remove connection if place was reserved.
func (l *connList) Reserve() bool {
	size := atomic.AddInt32(&l.size, 1)
	reserved := size <= l.max
	if !reserved {
		atomic.AddInt32(&l.size, -1)
	}
	return reserved
}

// Add adds connection to the list. The caller must reserve place first.
func (l *connList) Add(cn *conn) {
	l.mx.Lock()
	l.cns[cn] = struct{}{}
	l.mx.Unlock()
}

// Remove closes connection and removes it from the list.
func (l *connList) Remove(cn *conn) error {
	if cn == nil {
		atomic.AddInt32(&l.size, -1)
		return nil
	}

	l.mx.Lock()
	defer l.mx.Unlock()

	if _, ok := l.cns[cn]; ok {
		delete(l.cns, cn)
		atomic.AddInt32(&l.size, -1)
		return cn.Close()
	}

	if l.closed() {
		return nil
	}
	panic("conn not found in the list")
}

// Replace a connection with an old connection with a new one
func (l *connList) Replace(cn, newcn *conn) error {
	l.mx.Lock()
	defer l.mx.Unlock()

	if _, ok := l.cns[cn]; ok {
		delete(l.cns, cn)
		l.cns[newcn] = struct{}{}
		return cn.Close()
	}

	if l.closed() {
		return newcn.Close()
	}
	panic("conn not found in the list")
}

func (l *connList) Close() (retErr error) {
	l.mx.Lock()
	defer l.mx.Unlock()

	for cn := range l.cns {
		if err := cn.Close(); err != nil {
			retErr = err
		}
		delete(l.cns, cn)
	}
	l.cns = nil
	atomic.StoreInt32(&l.size, 0)
	return retErr
}

// Checks if closed, must be protected by mutex
func (l *connList) closed() bool {
	return l.cns == nil
}

type connPool struct {
	dialer func() (*conn, error)

	rl        *ratelimit.RateLimiter
	opt       *Options
	conns     *connList
	freeConns *connStack
	stats     PoolStats

	_closed int32

	lastErr atomic.Value
}

func newConnPool(opt *Options) *connPool {
	poolSize := opt.getPoolSize()
	p := &connPool{
		dialer: newConnDialer(opt),

		rl:        ratelimit.New(3*poolSize, time.Second),
		opt:       opt,
		conns:     newConnList(poolSize),
		freeConns: newConnStack(poolSize),
	}
	if p.opt.getIdleTimeout() > 0 {
		go p.reaper()
	}
	return p
}

func (p *connPool) closed() bool {
	return atomic.LoadInt32(&p._closed) == 1
}

func (p *connPool) isIdle(cn *conn) bool {
	return p.opt.getIdleTimeout() > 0 && time.Since(cn.UsedAt) > p.opt.getIdleTimeout()
}

// First returns first non-idle connection from the pool or nil if
// there are no connections.
func (p *connPool) First() *conn {
	for {
		cn := p.freeConns.Pop()
		if cn != nil && p.isIdle(cn) {
			var err error
			cn, err = p.replace(cn)
			if err != nil {
				Logger.Printf("pool.replace failed: %s", err)
				continue
			}
		}
		return cn
	}
	panic("not reached")
}

// wait waits for free non-idle connection. It returns nil on timeout.
func (p *connPool) wait() *conn {
	for {
		cn := p.freeConns.PopWithTimeout(p.opt.getPoolTimeout())
		if cn != nil && p.isIdle(cn) {
			var err error
			cn, err = p.replace(cn)
			if err != nil {
				Logger.Printf("pool.replace failed: %s", err)
				continue
			}
		}
		return cn
	}
	panic("not reached")
}

// Establish a new connection
func (p *connPool) new() (*conn, error) {
	if p.rl.Limit() {
		err := fmt.Errorf(
			"redis: you open connections too fast (last_error=%q)",
			p.loadLastErr(),
		)
		return nil, err
	}

	cn, err := p.dialer()
	if err != nil {
		p.storeLastErr(err.Error())
		return nil, err
	}

	return cn, nil
}

// Get returns existed connection from the pool or creates a new one.
func (p *connPool) Get() (cn *conn, isNew bool, err error) {
	if p.closed() {
		err = errClosed
		return
	}

	atomic.AddUint32(&p.stats.Requests, 1)

	// Fetch first non-idle connection, if available.
	if cn = p.First(); cn != nil {
		atomic.AddUint32(&p.stats.Hits, 1)
		return
	}

	// Try to create a new one.
	if p.conns.Reserve() {
		isNew = true

		cn, err = p.new()
		if err != nil {
			p.conns.Remove(nil) // decrease pool size
			return
		}
		p.conns.Add(cn)
		return
	}

	// Otherwise, wait for the available connection.
	atomic.AddUint32(&p.stats.Waits, 1)
	if cn = p.wait(); cn != nil {
		return
	}

	atomic.AddUint32(&p.stats.Timeouts, 1)
	err = errPoolTimeout
	return
}

func (p *connPool) Put(cn *conn) error {
	if cn.rd.Buffered() != 0 {
		b, _ := cn.rd.Peek(cn.rd.Buffered())
		err := fmt.Errorf("connection has unread data: %q", b)
		Logger.Print(err)
		return p.Remove(cn, err)
	}
	p.freeConns.Push(cn)
	return nil
}

func (p *connPool) replace(cn *conn) (*conn, error) {
	newcn, err := p.new()
	if err != nil {
		_ = p.conns.Remove(cn)
		return nil, err
	}
	_ = p.conns.Replace(cn, newcn)
	return newcn, nil
}

func (p *connPool) Remove(cn *conn, reason error) error {
	p.storeLastErr(reason.Error())

	// Replace existing connection with new one and unblock waiter.
	newcn, err := p.replace(cn)
	if err != nil {
		return err
	}
	p.freeConns.Push(newcn)
	return nil
}

// Len returns total number of connections.
func (p *connPool) Len() int {
	return p.conns.Len()
}

// FreeLen returns number of free connections.
func (p *connPool) FreeLen() int {
	return p.freeConns.Len()
}

func (p *connPool) Stats() *PoolStats {
	stats := p.stats
	stats.Requests = atomic.LoadUint32(&p.stats.Requests)
	stats.Waits = atomic.LoadUint32(&p.stats.Waits)
	stats.Timeouts = atomic.LoadUint32(&p.stats.Timeouts)
	stats.TotalConns = uint32(p.Len())
	stats.FreeConns = uint32(p.FreeLen())
	return &stats
}

func (p *connPool) Close() (retErr error) {
	if !atomic.CompareAndSwapInt32(&p._closed, 0, 1) {
		return errClosed
	}
	// Wait for app to free connections, but don't close them immediately.
	for i := 0; i < p.Len(); i++ {
		if cn := p.wait(); cn == nil {
			break
		}
	}
	// Close all connections.
	if err := p.conns.Close(); err != nil {
		retErr = err
	}
	return retErr
}

func (p *connPool) reaper() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for _ = range ticker.C {
		if p.closed() {
			break
		}

		// pool.First removes idle connections from the pool and
		// returns first non-idle connection. So just put returned
		// connection back.
		if cn := p.First(); cn != nil {
			p.Put(cn)
		}
	}
}

func (p *connPool) storeLastErr(err string) {
	p.lastErr.Store(err)
}

func (p *connPool) loadLastErr() string {
	if v := p.lastErr.Load(); v != nil {
		return v.(string)
	}
	return ""
}

//------------------------------------------------------------------------------

type singleConnPool struct {
	cn *conn
}

func newSingleConnPool(cn *conn) *singleConnPool {
	return &singleConnPool{
		cn: cn,
	}
}

func (p *singleConnPool) First() *conn {
	return p.cn
}

func (p *singleConnPool) Get() (*conn, bool, error) {
	return p.cn, false, nil
}

func (p *singleConnPool) Put(cn *conn) error {
	if p.cn != cn {
		panic("p.cn != cn")
	}
	return nil
}

func (p *singleConnPool) Remove(cn *conn, _ error) error {
	if p.cn != cn {
		panic("p.cn != cn")
	}
	return nil
}

func (p *singleConnPool) Len() int {
	return 1
}

func (p *singleConnPool) FreeLen() int {
	return 0
}

func (p *singleConnPool) Stats() *PoolStats { return nil }

func (p *singleConnPool) Close() error {
	return nil
}

//------------------------------------------------------------------------------

type stickyConnPool struct {
	pool     pool
	reusable bool

	cn     *conn
	closed bool
	mx     sync.Mutex
}

func newStickyConnPool(pool pool, reusable bool) *stickyConnPool {
	return &stickyConnPool{
		pool:     pool,
		reusable: reusable,
	}
}

func (p *stickyConnPool) First() *conn {
	p.mx.Lock()
	cn := p.cn
	p.mx.Unlock()
	return cn
}

func (p *stickyConnPool) Get() (cn *conn, isNew bool, err error) {
	p.mx.Lock()
	defer p.mx.Unlock()

	if p.closed {
		err = errClosed
		return
	}
	if p.cn != nil {
		cn = p.cn
		return
	}

	cn, isNew, err = p.pool.Get()
	if err != nil {
		return
	}
	p.cn = cn
	return
}

func (p *stickyConnPool) put() (err error) {
	err = p.pool.Put(p.cn)
	p.cn = nil
	return err
}

func (p *stickyConnPool) Put(cn *conn) error {
	p.mx.Lock()
	defer p.mx.Unlock()

	if p.closed {
		return errClosed
	}
	if p.cn != cn {
		panic("p.cn != cn")
	}
	return nil
}

func (p *stickyConnPool) remove(reason error) error {
	err := p.pool.Remove(p.cn, reason)
	p.cn = nil
	return err
}

func (p *stickyConnPool) Remove(cn *conn, reason error) error {
	p.mx.Lock()
	defer p.mx.Unlock()

	if p.closed {
		return errClosed
	}
	if p.cn == nil {
		panic("p.cn == nil")
	}
	if cn != nil && p.cn != cn {
		panic("p.cn != cn")
	}
	return p.remove(reason)
}

func (p *stickyConnPool) Len() int {
	p.mx.Lock()
	defer p.mx.Unlock()

	if p.cn == nil {
		return 0
	}
	return 1
}

func (p *stickyConnPool) FreeLen() int {
	p.mx.Lock()
	defer p.mx.Unlock()

	if p.cn == nil {
		return 1
	}
	return 0
}

func (p *stickyConnPool) Stats() *PoolStats { return nil }

func (p *stickyConnPool) Close() error {
	p.mx.Lock()
	defer p.mx.Unlock()

	if p.closed {
		return errClosed
	}
	p.closed = true
	var err error
	if p.cn != nil {
		if p.reusable {
			err = p.put()
		} else {
			reason := errors.New("redis: sticky not reusable connection")
			err = p.remove(reason)
		}
	}
	return err
}
