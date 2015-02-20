// Package cluster implements an almost drop-in replacement for a normal Client
// which accounts for a redis cluster setup. It will transparently redirect
// requests to the correct nodes, as well as keep track of which slots are
// mapped to which nodes and updating them accordingly so requests can remain as
// fast as possible.
//
// This package will initially call `cluster slots` in order to retrieve an
// initial idea of the topology of the cluster, but other than that will not
// make any other extraneous calls.
//
// All methods on a Cluster are thread-safe, and connections are automatically
// pooled
package cluster

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/mediocregopher/radix.v2/pool"
	"github.com/mediocregopher/radix.v2/redis"
)

const numSlots = 16384

type mapping [numSlots]string

func errorResp(err error) *redis.Resp {
	return redis.NewResp(err)
}

func errorRespf(format string, args ...interface{}) *redis.Resp {
	return errorResp(fmt.Errorf(format, args...))
}

var (
	// ErrBadCmdNoKey is an error reply returned when no key is given to the Cmd
	// method
	ErrBadCmdNoKey = errors.New("bad command, no key")

	errNoPools = errors.New("no pools to pull from")
)

// Cluster wraps a Client and accounts for all redis cluster logic
type Cluster struct {
	o Opts
	mapping
	pools         map[string]*pool.Pool
	resetThrottle *time.Ticker
	callCh        chan func(*Cluster)
	stopCh        chan struct{}

	// This is written to whenever a slot miss (either a MOVED or ASK) is
	// encountered. This is mainly for informational purposes, it's not meant to
	// be actionable. If nothing is listening the message is dropped
	MissCh chan struct{}

	// This is written to whenever the cluster discovers there's been some kind
	// of re-ordering/addition/removal of cluster nodes. If nothing is listening
	// the message is dropped
	ChangeCh chan struct{}
}

// Opts are Options which can be passed in to NewClusterWithOpts. If any
// are set to their zero value the default value will be used instead
type Opts struct {

	// Required. The address of a single node in the cluster
	Addr string

	// Read and write timeout which should be used on individual redis clients.
	// Default is to not set the timeout and let the connection use it's default
	Timeout time.Duration

	// The size of the connection pool to use for each host. Default is 10
	PoolSize int

	// The time which must elapse between subsequent calls to Reset(). The
	// default is 10 seconds
	ResetThrottle time.Duration
}

// New will perform the following steps to initialize:
//
// - Connect to the node given in the argument
//
// - Use that node to call CLUSTER SLOTS. The return from this is used to build
// a mapping of slot number -> connection. At the same time any new connections
// which need to be made are created here.
//
// - *Cluster is returned
//
// At this point the Cluster has a complete view of the cluster's topology and
// can immediately start performing commands with (theoretically) zero slot
// misses
func New(addr string) (*Cluster, error) {
	return NewWithOpts(Opts{
		Addr: addr,
	})
}

// NewWithOpts is the same as NewCluster, but with more fine-tuned
// configuration options. See Opts for more available options
func NewWithOpts(o Opts) (*Cluster, error) {
	if o.PoolSize == 0 {
		o.PoolSize = 10
	}
	if o.ResetThrottle == 0 {
		o.ResetThrottle = 10 * time.Second
	}

	initialPool, err := newPool(o.Addr, &o)
	if err != nil {
		return nil, err
	}

	c := Cluster{
		o:       o,
		mapping: mapping{},
		pools: map[string]*pool.Pool{
			o.Addr: initialPool,
		},
		callCh:   make(chan func(*Cluster)),
		stopCh:   make(chan struct{}),
		MissCh:   make(chan struct{}),
		ChangeCh: make(chan struct{}),
	}
	go c.spin()
	if err := c.Reset(); err != nil {
		return nil, err
	}
	return &c, nil
}

func newPool(addr string, o *Opts) (*pool.Pool, error) {
	df := func(network, addr string) (*redis.Client, error) {
		return redis.DialTimeout(network, addr, o.Timeout)
	}
	return pool.NewCustom("tcp", addr, o.PoolSize, df)
}

// Anything which requires creating/deleting pools must be done in here
func (c *Cluster) spin() {
	for {
		select {
		case f := <-c.callCh:
			f(c)
		case <-c.stopCh:
			return
		}
	}
}

// Returns a connection for the given key or given address, depending on which
// is set. If the given pool couldn't be used a connection from a random pool
// will (attempt) to be returned
func (c *Cluster) getConn(key, addr string) (*redis.Client, error) {
	type resp struct {
		conn *redis.Client
		err  error
	}
	respCh := make(chan *resp)
	c.callCh <- func(c *Cluster) {
		if key != "" {
			addr = c.addrForKeyInner(key)
		}

		var err error
		p, ok := c.pools[addr]
		if !ok {
			p, err = newPool(addr, &c.o)
		}

		var conn *redis.Client
		if err == nil {
			conn, err = p.Get()
			if err == nil {
				respCh <- &resp{conn, nil}
				return
			}
		}

		// If there's an error try one more time retrieving from a random pool
		// before bailing
		p = c.getRandomPoolInner()
		if p == nil {
			respCh <- &resp{err: errNoPools}
			return
		}
		conn, err = p.Get()
		if err != nil {
			respCh <- &resp{err: err}
			return
		}

		respCh <- &resp{conn, nil}
	}
	r := <-respCh
	return r.conn, r.err
}

// Put putss the connection back in its pool. To be used alongside any of the
// Get* methods once use of the redis.Client is done
func (c *Cluster) Put(conn *redis.Client) {
	c.callCh <- func(c *Cluster) {
		p := c.pools[conn.Addr]
		if p == nil {
			conn.Close()
			return
		}

		p.Put(conn)
	}
}

func (c *Cluster) getRandomPoolInner() *pool.Pool {
	for _, pool := range c.pools {
		return pool
	}
	return nil
}

// Reset will re-retrieve the cluster topology and set up/teardown connections
// as necessary. It begins by calling CLUSTER SLOTS on a random known
// connection. The return from that is used to re-create the topology, create
// any missing clients, and close any clients which are no longer needed.
//
// This call is inherently throttled, so that multiple clients can call it at
// the same time and it will only actually occur once (subsequent clients will
// have nil returned immediately).
func (c *Cluster) Reset() error {
	respCh := make(chan error)
	c.callCh <- func(c *Cluster) {
		respCh <- c.resetInner()
	}
	return <-respCh
}

func (c *Cluster) resetInner() error {

	// Throttle resetting so a bunch of routines can call Reset at once and the
	// server won't be spammed. We don't a throttle until the second Reset is
	// called, so the initial call inside New goes through correctly
	if c.resetThrottle != nil {
		select {
		case <-c.resetThrottle.C:
		default:
			return nil
		}
	} else {
		c.resetThrottle = time.NewTicker(c.o.ResetThrottle)
	}

	p := c.getRandomPoolInner()
	if p == nil {
		return fmt.Errorf("no available nodes to call CLUSTER SLOTS on")
	}

	client, err := p.Get()
	if err != nil {
		return err
	}
	defer client.Close()

	pools := map[string]*pool.Pool{
		p.Addr: p,
	}

	elems, err := client.Cmd("CLUSTER", "SLOTS").Array()
	if err != nil {
		return err
	} else if len(elems) == 0 {
		return errors.New("empty CLUSTER SLOTS response")
	}

	var start, end, port int
	var ip, slotAddr string
	var slotPool *pool.Pool
	var ok, changed bool
	for _, slotGroup := range elems {
		slotElems, err := slotGroup.Array()
		if err != nil {
			return err
		}
		if start, err = slotElems[0].Int(); err != nil {
			return err
		}
		if end, err = slotElems[1].Int(); err != nil {
			return err
		}
		slotAddrElems, err := slotElems[2].Array()
		if err != nil {
			return err
		}
		if ip, err = slotAddrElems[0].Str(); err != nil {
			return err
		}
		if port, err = slotAddrElems[1].Int(); err != nil {
			return err
		}

		// cluster slots returns a blank ip for the node we're currently
		// connected to. I guess the node doesn't know its own ip? I guess that
		// makes sense
		if ip == "" {
			slotAddr = p.Addr
		} else {
			slotAddr = ip + ":" + strconv.Itoa(port)
		}
		for i := start; i <= end; i++ {
			c.mapping[i] = slotAddr
		}
		if slotPool, ok = c.pools[slotAddr]; ok {
			pools[slotAddr] = slotPool
		} else {
			slotPool, err = newPool(slotAddr, &c.o)
			if err != nil {
				return err
			}
			changed = true
			pools[slotAddr] = slotPool
		}
	}

	for addr := range c.pools {
		if _, ok := pools[addr]; !ok {
			c.pools[addr].Empty()
			changed = true
		}
	}
	c.pools = pools

	if changed {
		select {
		case c.ChangeCh <- struct{}{}:
		default:
		}
	}

	return nil
}

// Logic for doing a command:
// * Get client for command's slot, try it
// * If err == nil, return reply
// * If err is a client error:
// 		* If MOVED:
//			* If node not tried before, go to top with that node
//			* Otherwise if we haven't Reset, do that and go to top with random
//			  node
//			* Otherwise error out
//		* If ASK (same as MOVED, but call ASKING beforehand and don't modify
//		  slots)
// 		* Otherwise return the error
// * Otherwise it is a network error
//		* If we haven't reconnected to this node yet, do that and go to top
//		* If we haven't reset yet do that, pick a random node, and go to top
//		* Otherwise return network error (we don't reset, we have no nodes to do
//		  it with)

// Cmd performs the given command on the correct cluster node and gives back the
// command's reply. The command *must* have a key parameter (i.e. len(args) >=
// 1). If any MOVED or ASK errors are returned they will be transparently
// handled by this method.
func (c *Cluster) Cmd(cmd string, args ...interface{}) *redis.Resp {
	if len(args) < 1 {
		return errorResp(ErrBadCmdNoKey)
	}

	key, err := KeyFromArgs(args)
	if err != nil {
		return errorResp(err)
	}

	client, err := c.getConn(key, "")
	if err != nil {
		return errorResp(err)
	}

	return c.clientCmd(client, cmd, args, false, nil, false)
}

func haveTried(tried map[string]bool, addr string) bool {
	if tried == nil {
		return false
	}
	return tried[addr]
}

func justTried(tried map[string]bool, addr string) map[string]bool {
	if tried == nil {
		tried = map[string]bool{}
	}
	tried[addr] = true
	return tried
}

func (c *Cluster) clientCmd(
	client *redis.Client, cmd string, args []interface{}, ask bool,
	tried map[string]bool, haveReset bool,
) *redis.Resp {
	var err error
	var r *redis.Resp
	defer c.Put(client)

	if ask {
		r = client.Cmd("ASKING")
		ask = false
	}

	// If we asked and got an error, we continue on with error handling as we
	// would normally do. If we didn't ask or the ask succeeded we do the
	// command normally, and see how that goes
	if r == nil || r.Err == nil {
		r = client.Cmd(cmd, args...)
	}

	if err = r.Err; err == nil {
		return r
	}

	// At this point we have some kind of error we have to deal with. The above
	// code is what will be run 99% of the time and is pretty streamlined,
	// everything after this point is allowed to be hairy and gross

	haveTriedBefore := haveTried(tried, client.Addr)
	tried = justTried(tried, client.Addr)

	// Deal with n
	if r.IsType(redis.IOErr) {
		// If this is the first time trying this node, try it again
		if !haveTriedBefore {
			if client, try2err := c.getConn("", client.Addr); try2err == nil {
				return c.clientCmd(client, cmd, args, false, tried, haveReset)
			}
		}
		// Otherwise try calling Reset() and getting a random client
		if !haveReset {
			if resetErr := c.Reset(); resetErr != nil {
				return errorRespf("Could not get cluster info: %s", resetErr)
			}
			client, getErr := c.getConn("", "")
			if getErr != nil {
				return errorResp(getErr)
			}
			return c.clientCmd(client, cmd, args, false, tried, true)
		}
		// Otherwise give up and return the most recent error
		return r
	}

	// Here we deal with application errors that are either MOVED or ASK
	msg := err.Error()
	moved := strings.HasPrefix(msg, "MOVED ")
	ask = strings.HasPrefix(msg, "ASK ")
	if moved || ask {
		slot, addr := redirectInfo(msg)
		c.callCh <- func(c *Cluster) {
			select {
			case c.MissCh <- struct{}{}:
			default:
			}
		}

		// if we already tried the node we've been told to try, Reset and
		// try again with a random node. If that still doesn't work, or we
		// already did that once, bail hard
		if haveTried(tried, addr) {
			if haveReset {
				return errorRespf("Cluster doesn't make sense")
			}
			if resetErr := c.Reset(); resetErr != nil {
				return errorRespf("Could not get cluster info: %s", resetErr)
			}
			client, getErr := c.getConn("", "")
			if getErr != nil {
				return errorRespf("No available cluster nodes: %s", getErr)
			}

			// we go back to scratch here, pretend we haven't tried any
			// since we just picked a random node, it's likely we'll get a
			// redirect. We won't reset again so this doesn't hurt too much
			return c.clientCmd(client, cmd, args, false, nil, true)

			// We don't want to change the slot if we've tried this address for
			// this slot before, it changed it the last time probably and
			// obiously it doesn't work anyway
		} else if moved {
			c.callCh <- func(c *Cluster) {
				c.mapping[slot] = addr
			}
		}

		client, getErr := c.getConn("", addr)
		if getErr != nil {
			return errorResp(getErr)
		}
		return c.clientCmd(client, cmd, args, ask, tried, haveReset)
	}

	// It's a normal application error (like WRONG KEY TYPE or whatever), return
	// that to the client
	return r
}

func redirectInfo(msg string) (int, string) {
	parts := strings.Split(msg, " ")
	slotStr := parts[1]
	slot, err := strconv.Atoi(slotStr)
	if err != nil {
		// if redis is returning bad integers, we have problems
		panic(err)
	}
	addr := parts[2]
	return slot, addr
}

// KeyFromArgs takes in the args parameters that might be passed into Cmd and
// returns the key that will be chosen for that set of arguments to locate the
// command in the cluster. Since radix supports complicated arguments (like
// slices, slices of slices, maps, etc...) this is not always as straightforward
// as it might seem, so this helper method is provided.
//
// ErrBadCmdNoKey is returned if no key can be determined
func KeyFromArgs(args ...interface{}) (string, error) {
	if len(args) == 0 {
		return "", ErrBadCmdNoKey
	}
	arg := args[0]
	switch argv := arg.(type) {
	case string:
		return argv, nil
	case []byte:
		return string(argv), nil
	default:
		switch reflect.TypeOf(arg).Kind() {
		case reflect.Slice:
			argVal := reflect.ValueOf(arg)
			if argVal.Len() < 1 {
				return "", ErrBadCmdNoKey
			}
			first := argVal.Index(0).Interface()
			return KeyFromArgs(first)
		case reflect.Map:
			// Maps have no order, we can't possibly choose a key out of one
			return "", ErrBadCmdNoKey
		default:
			return fmt.Sprint(arg), nil
		}
	}
}

func (c *Cluster) addrForKeyInner(key string) string {
	if start := strings.Index(key, "{"); start >= 0 {
		if end := strings.Index(key[start+2:], "}"); end >= 0 {
			key = key[start+1 : start+2+end]
		}
	}
	i := CRC16([]byte(key)) % numSlots
	return c.mapping[i]
}

// GetForKey returns the Client which *ought* to handle the given key, based
// on Cluster's understanding of the cluster topology at the given moment. If
// the slot isn't known or there is an error contacting the correct node, a
// random client is returned. The client must be returned back to its pool using
// Put when through
func (c *Cluster) GetForKey(key string) (*redis.Client, error) {
	return c.getConn(key, "")
}

// GetEvery returns a single *redis.Client per master that the cluster currently
// knows about. The map returned maps the address of the client to the client
// itself. If there is an error retrieving any of the clients (for instance if a
// new connection has to be made to get it) only that error is returned. Each
// client must be returned back to its pools using Put when through
func (c *Cluster) GetEvery() (map[string]*redis.Client, error) {
	type resp struct {
		m   map[string]*redis.Client
		err error
	}
	respCh := make(chan resp)
	c.callCh <- func(c *Cluster) {
		m := map[string]*redis.Client{}
		for addr, p := range c.pools {
			client, err := p.Get()
			if err != nil {
				respCh <- resp{nil, err}
				return
			}
			m[addr] = client
		}
		respCh <- resp{m, nil}
	}

	r := <-respCh
	return r.m, r.err
}

// GetAddrForKey returns the address which would be used to handle the given key
// in the cluster.
func (c *Cluster) GetAddrForKey(key string) string {
	respCh := make(chan string)
	c.callCh <- func(c *Cluster) {
		respCh <- c.addrForKeyInner(key)
	}
	return <-respCh
}

// Close calls Close on all connected clients. Once this is called no other
// methods should be called on this instance of Cluster
func (c *Cluster) Close() {
	c.callCh <- func(c *Cluster) {
		for addr, p := range c.pools {
			p.Empty()
			delete(c.pools, addr)
		}
		if c.resetThrottle != nil {
			c.resetThrottle.Stop()
		}
	}
	close(c.stopCh)
}