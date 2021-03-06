package kcp

import (
    "crypto/rand"
    "encoding/binary"
    "net"
    "sync"
    "sync/atomic"
    "time"
    "errors"
    "log"
    "math"
    "io"
)

type errTimeout struct {
    error
}

func (errTimeout) Timeout() bool   { return true }
func (errTimeout) Temporary() bool { return true }
func (errTimeout) Error() string   { return "i/o timeout" }

const (
    defaultWndSize           = 128 // default window size, in packet
    udpPacketSizeLimit       = 2048
    rxQueueLimit             = 8192
    defaultKeepAliveInterval = 10
    kcpSendBufferLimit       = 1024 * 1024
)

const (
    errBrokenPipe       = "broken pipe"
    errInvalidOperation = "invalid operation"
)

var (
    udpPacketPool sync.Pool
)

func init() {
    udpPacketPool.New = func() interface{} {
        return make([]byte, udpPacketSizeLimit)
    }
}

type (
    // KCPConn defines a KCP session implemented by UDP
    KCPConn struct {
        kcp        *KCP      // the core ARQ
        listener   *Listener // point to server listener if it's a server socket
        isClient   bool
        conn       net.PacketConn // the underlying packet socket
        remoteAddr net.Addr

        deadlineRead  time.Time
        deadlineWrite time.Time

        bufRead *Buffer

        die             chan struct{}
        chReadEvent     chan struct{}
        chWriteEvent    chan struct{}
        chKcpFlushEvent chan struct{}
        chUdpInput      chan []byte

        keepAliveInterval int32
        keepAliveTimer    *time.Timer
        mu                sync.Mutex
    }

    setReadBuffer interface {
        SetReadBuffer(bytes int) error
    }

    setWriteBuffer interface {
        SetWriteBuffer(bytes int) error
    }
)

// newKCPConn create a new udp session for client or server
func newKCPConn() *KCPConn {
    s := new(KCPConn)

    s.bufRead = new(Buffer)

    s.die = make(chan struct{})
    s.chReadEvent = make(chan struct{}, 1)
    s.chWriteEvent = make(chan struct{}, 1)
    s.chKcpFlushEvent = make(chan struct{}, 1)
    s.chUdpInput = make(chan []byte, rxQueueLimit)

    s.keepAliveInterval = defaultKeepAliveInterval
    s.keepAliveTimer = time.NewTimer(s.getKeepAliveInterval())

    s.kcp = newKCP()
    s.kcp.setWndSize(defaultWndSize, defaultWndSize)
    s.kcp.setMtu(IKCP_MTU_DEF)
    s.kcp.stream = 1
    s.kcp.output = s.kcpOutput
    s.kcp.stats = &KcpConnStats{}
    s.kcp.stats.StartTime = 0
    s.kcp.stats.EndTime = 0

    //go s.debug()
    return s
}

func (s *KCPConn) accept(conv uint32, l *Listener, conn net.PacketConn, remote net.Addr) {
    s.listener = l
    s.isClient = false
    s.conn = conn
    s.remoteAddr = remote
    s.kcp.conv = conv
    atomic.AddInt64(&Stats.TotalAccept, 1)
    go s.run();
}

func (s *KCPConn) connect(conv uint32, conn net.PacketConn, remote net.Addr) {
    s.isClient = true
    s.conn = conn
    s.remoteAddr = remote
    s.kcp.conv = conv
    atomic.AddInt64(&Stats.TotalConnect, 1)

    s.goRunClientRecv()
    go s.run();
    s.mu.Lock()
    s.kcp.sendConnectFlush(currentTickMs())
    s.mu.Unlock()
    <-s.chWriteEvent
}

func (s *KCPConn) Dump() {
    s.mu.Lock()
    role := "server"
    if s.isClient {
        role = "client"
    }
    s.kcp.stats.AliveTime = s.kcp.stats.EndTime - s.kcp.stats.StartTime
    log.Printf("-- role=%s (%p) conv=%d -- stats: %+v, isLocalOpen=%v, isRemoteOpen=%v, snd_nxt=%d, WaitSnd=%d, rcv_nxt=%d, rcv_queue=%d, rcv_buf=%d\n",
        role, s, s.GetConv(), s.kcp.stats, !s.kcp.isStateLocalClosed(), s.kcp.isRemoteOpen(), s.kcp.snd_nxt, s.kcp.waitSnd(), s.kcp.rcv_nxt, len(s.kcp.rcv_queue), len(s.kcp.rcv_buf))
    log.Println()
    s.kcp.stats = &KcpConnStats{}
    s.mu.Unlock()
}

func (s *KCPConn) debug() {
loop:
    for {
        select {
        case <-s.die:
            log.Printf("%p stats: %+v\n", s, s.kcp.stats)
            log.Printf("%p close\n", s)
            break loop

        case <-time.After(time.Second):
            s.Dump()
        }
    }
}

func (s *KCPConn) getKeepAliveInterval() (time.Duration) {
    if s.keepAliveInterval == 0 {
        return math.MaxInt32 * time.Second
    } else {
        return time.Duration(s.keepAliveInterval) * time.Second
    }
}

// Read implements the Conn Read method.
func (s *KCPConn) Read(b []byte) (int, error) {
    for {
        lockTime := currentTickMs()
        s.mu.Lock()
        lockTime = currentTickMs() - lockTime
        if lockTime > 10 {
            log.Printf("conv: %d read lock time: %d", s.GetConv(), lockTime)
        }
        s.keepAliveTimer.Reset(s.getKeepAliveInterval())
        if s.bufRead.Len() < len(b) {
            for {
                n := s.kcp.recvSize();
                if n <= 0 {
                    break
                }

                s.kcp.stats.EndTime = currentTickMs()
                if s.kcp.stats.StartTime == 0 {
                    s.kcp.stats.StartTime = currentTickMs()
                }
                s.kcp.recv(s.bufRead.Extend(n))
                s.kcp.stats.ByteRx += int64(n)
                atomic.AddInt64(&Stats.ByteRx, int64(n))
            }
        }

        shouldClose := s.kcp.shouldClose()
        if shouldClose {
            log.Printf("Read call closeInternal udp local %s conv %d because shouldClose", s.conn.LocalAddr(), s.GetConv())
            s.closeInternal()
        }

        if s.bufRead.Len() > 0 {
            n, err := s.bufRead.Read(b)
            s.mu.Unlock()
            return n, err
        }

        if s.kcp.isStateLocalClosed() {
            s.mu.Unlock()
            return 0, io.EOF
        }

        var timeout *time.Timer
        var deadline <-chan time.Time
        if !s.deadlineRead.IsZero() {
            delay := s.deadlineRead.Sub(time.Now())
            if delay <= 0 {
                s.mu.Unlock()
                return 0, errTimeout{}
            }
            timeout = time.NewTimer(delay)
            deadline = timeout.C
        }
        s.mu.Unlock()

        // wait for read event or timeout
        select {
        case <-s.chReadEvent:
        case <-deadline:
        case <-s.die:
        case <-s.keepAliveTimer.C:
            if s.keepAliveInterval != 0 {
                s.keepAliveTimer.Stop()
                log.Printf("Read call Close udp local %s conv %d because keep alive timeout", s.conn.LocalAddr(), s.GetConv())
                s.Close()
            }
        }

        if timeout != nil {
            timeout.Stop()
        }
    }
}

func (s *KCPConn) canKcpSendInternal() bool {
    return s.kcp.waitSnd() < int(s.kcp.snd_wnd) && s.kcp.isStateConnected()
}

// Write implements the Conn Write method.
func (s *KCPConn) Write(b []byte) (int, error) {
    for {
        lockTime := currentTickMs()
        s.mu.Lock()
        lockTime = currentTickMs() - lockTime
        if lockTime > 10 {
            log.Printf("conv: %d write lock time: %d", s.GetConv(), lockTime)
        }
        s.keepAliveTimer.Reset(s.getKeepAliveInterval())
        if s.kcp.isStateLocalClosed() {
            s.mu.Unlock()
            return 0, errors.New(errBrokenPipe)
        }

        if s.canKcpSendInternal() {
            if s.kcp.stats.StartTime == 0 {
                s.kcp.stats.StartTime = currentTickMs()
            }
            //log.Printf("conv: %d write call send", s.GetConv())
            ret := s.kcp.send(b)
            s.mu.Unlock()

            if ret < 0 {
                log.Panicf("internal bug, kcp send: %d", ret)
            }

            s.kcp.stats.ByteTx += int64(len(b))
            atomic.AddInt64(&Stats.ByteTx, int64(len(b)))

            if s.kcp.sndBufAvail() > 0 {
                s.notifyFlushEvent()
            }

            return len(b), nil
        }

        var timeout *time.Timer
        var deadline <-chan time.Time
        if !s.deadlineWrite.IsZero() {
            delay := s.deadlineWrite.Sub(time.Now())
            if delay <= 0 {
                s.mu.Unlock()
                return 0, errTimeout{}
            }

            timeout = time.NewTimer(delay)
            deadline = timeout.C
        }
        s.mu.Unlock()

        // wait for write event or timeout
        select {
        case <-s.chWriteEvent:
        case <-deadline:
        case <-s.die:
        case <-s.keepAliveTimer.C:
            if s.keepAliveInterval != 0 {
                s.keepAliveTimer.Stop()
                log.Printf("Write call Close udp local %s conv %d because keep alive timeout", s.conn.LocalAddr(), s.GetConv())
                s.Close()
            }
        }

        if timeout != nil {
            timeout.Stop()
        }
    }
}

// Close closes the connection.
func (s *KCPConn) closeInternal() error {
    if s.conn == nil || s.kcp.isStateLocalClosed() {
        return errors.New(errBrokenPipe)
    }

    close(s.die)
    log.Printf("closeInternal call sendCloseFlush udp local %s conv %d ", s.conn.LocalAddr(), s.GetConv())
    s.kcp.sendCloseFlush(currentTickMs())
    //if s.isClient {
    //	s.conn.Close()
    //}
    return nil
}

// Close closes the connection.
func (s *KCPConn) Close() error {
    s.mu.Lock()
    defer s.mu.Unlock()
    log.Printf("Close called conv:%d", s.kcp.conv)
    return s.closeInternal()
}

func (s *KCPConn) IsClosed() bool {
    s.mu.Lock()
    defer s.mu.Unlock()

    return !s.kcp.isAllOpen()
}

func (s *KCPConn) doKcpInput(data []byte) bool {
    lockTime := currentTickMs()
    s.mu.Lock()
    lockTime = currentTickMs() - lockTime
    if lockTime > 10 {
        log.Printf("conv: %d doKcpInput lock time: %d", s.GetConv(), lockTime)
    }
    defer s.mu.Unlock()

    alreadyConnected := s.kcp.isStateConnected()
    if ret := s.kcp.input(currentTickMs(), data, true); ret != 0 {
        s.kcp.stats.ErrorInput += 1
        atomic.AddInt64(&Stats.ErrorInput, 1)
    }

    if !alreadyConnected && s.kcp.isStateConnected() {
        s.kcp.sendConnectFlush(currentTickMs())
        s.notifyWriteEvent()
    }

    n := s.kcp.recvSize()
    if n > 0 || s.kcp.shouldClose() {
        s.notifyReadEvent()
    }

    udpPacketPool.Put(data)

    s.kcp.stats.PacketIn += 1
    s.kcp.stats.ByteIn += int64(len(data))
    atomic.AddInt64(&Stats.PacketIn, 1)
    atomic.AddInt64(&Stats.ByteIn, int64(len(data)))
    return true
}

func (s *KCPConn) goRunClientRecv() {
    go func() {
        for {
            data := udpPacketPool.Get().([]byte)[:udpPacketSizeLimit]
            if n, _, err := s.conn.ReadFrom(data); err == nil {
                select {
                case s.chUdpInput <- data[:n]:
                case <-s.die:
                    return
                }
            } else if err != nil {
                //FIXME: what to do ?
                log.Printf("client=%v udp read error: %v\n", s.isClient, err)
                s.kcp.stats.ErrorRead += 1
                atomic.AddInt64(&Stats.ErrorRead, 1)
                return
            }
        }
    }()
}

func (s *KCPConn) run() {
    connCurrent := atomic.AddInt64(&Stats.ConnCurrent, 1)
    connMax := atomic.LoadInt64(&Stats.ConnMax)
    if connCurrent > connMax {
        atomic.CompareAndSwapInt64(&Stats.ConnMax, connMax, connCurrent)
    }

    // TODO: NAT keep alive
    //var lastPing time.Time
    //ticker := time.NewTicker(5 * time.Second)
    //defer ticker.Stop()

    // main loop
    updateDelayMax := 1000 * time.Millisecond
    updateDelayMin := 10 * time.Millisecond

    updateDelay := updateDelayMin

    doKcpFlush := false
    doKcpUpdate := false

    updateTimer := time.NewTimer(updateDelay)

loopMain:
    for s.kcp.isAllOpen() {

        select {
        case data := <-s.chUdpInput:
            //log.Printf("conv: %d run call flush because input", s.GetConv())
            s.doKcpInput(data)
            continue loopMain
        //updateDelay = updateDelay / 2

        case <-s.chKcpFlushEvent:
            //log.Printf("conv: %d run call flush because flush event", s.GetConv())
            doKcpFlush = true
        //updateDelay = updateDelay / 2

        case <-updateTimer.C:
            //log.Printf("conv: %d run call flush because update timer", s.GetConv())
            doKcpUpdate = true
        //updateDelay = updateDelay * 2

        case <-s.die:
            break loopMain
        }
        //if updateDelay > updateDelayMax {
        //    updateDelay = updateDelayMax
        //}
        //
        //if updateDelay < updateDelayMin {
        //    updateDelay = updateDelayMin
        //}

        updateTimer.Reset(updateDelay)

        lockTime := currentTickMs()
        s.mu.Lock()
        lockTime = currentTickMs() - lockTime
        if lockTime > 10 {
            log.Printf("conv: %d run 0 lock time: %d", s.GetConv(), lockTime)
        }

        //if s.kcp.sndBufAvail() > 0 {
        //	doKcpFlush = true
        //}

        //if doKcpFlush {
        //	log.Printf("conv: %d run call flush", s.GetConv())
        //	s.kcp.flush(currentTickMs())
        //} else {
        //	log.Printf("conv: %d run call update", s.GetConv())
        //	s.kcp.update(currentTickMs())
        //}

        if doKcpFlush {
            //log.Printf("conv: %d run call flush", s.GetConv())
            s.kcp.flush(currentTickMs())
        }
        if doKcpUpdate {
            //log.Printf("conv: %d run call update", s.GetConv())
            s.kcp.update(currentTickMs())
        }
        if s.canKcpSendInternal() {
            s.notifyWriteEvent()
        }
        s.mu.Unlock()

        doKcpFlush = false
        doKcpUpdate = false
        //select {
        //case data := <-s.chUdpInput:
        //	//log.Printf("conv: %d run call flush because input", s.GetConv())
        //	s.doKcpInput(data)
        ////updateDelay = updateDelay / 2
        //
        //case <-s.chKcpFlushEvent:
        //	//log.Printf("conv: %d run call flush because flush event", s.GetConv())
        //	doKcpFlush = true
        ////updateDelay = updateDelay / 2
        //
        //case <-updateTimer.C:
        ////log.Printf("conv: %d run call flush because update timer", s.GetConv())
        //	doKcpUpdate = true
        ////updateDelay = updateDelay * 2
        //
        //case <-s.die:
        //	break loopMain
        //}
    }

    atomic.AddInt64(&Stats.ConnClosing, 1)
    //fmt.Printf("kcp conn close\n")

    if !s.kcp.isStateLocalClosed() {
        log.Printf("run call sendCloseFlush udp local %s conv %d because remote closed but local is open", s.conn.LocalAddr(), s.GetConv())
        s.kcp.sendCloseFlush(currentTickMs())
    }

    var closeWaitStartTime time.Time
    dangling := true
loopClose:
    for {
        lockTime := currentTickMs()
        s.mu.Lock()
        lockTime = currentTickMs() - lockTime
        if lockTime > 10 {
            log.Printf("conv: %d run 1 lock time: %d", s.GetConv(), lockTime)
        }
        //log.Printf("conv: %d run close call update", s.GetConv())
        s.kcp.update(currentTickMs())
        isRemoteClosed := s.kcp.isStateRemoteClosed()
        isDead := s.kcp.isStateDead()
        isLocalClosed := s.kcp.isStateLocalClosed()
        if s.kcp.waitSnd() > 0 || !isRemoteClosed {
            updateDelay = 100 * time.Millisecond
        } else {
            updateDelay = updateDelayMax
            dangling = false
        }

        updateTimer.Reset(updateDelay)

        s.mu.Unlock()

        // local closed, do not wait for future packets, just close
        if isLocalClosed {
            break loopClose
        }

        if isDead {
            break loopClose
        }

        if isRemoteClosed {
            if closeWaitStartTime.IsZero() {
                closeWaitStartTime = time.Now()
            }

            //FIXME: consider monotonic clock
            if math.Abs(time.Now().Sub(closeWaitStartTime).Seconds()) >= 5 {
                break loopClose
            }
        }

        select {
        case data := <-s.chUdpInput:
            s.doKcpInput(data)

        case <-updateTimer.C:
        }
    }

    if s.listener == nil {
        //client
        //s.conn.Close()
    } else {
        //server
        select {
        case s.listener.chDeadConns <- s:
        case <-s.listener.die:
        }
    }

    atomic.AddInt64(&Stats.ConnCurrent, -1)
    atomic.AddInt64(&Stats.ConnClosing, -1)
    atomic.AddInt64(&Stats.TotalClose, 1)
    if dangling {
        atomic.AddInt64(&Stats.TotalCloseDangling, 1)
    }
}

// LocalAddr returns the local network address. The Addr returned is shared by all invocations of LocalAddr, so do not modify it.
func (s *KCPConn) LocalAddr() net.Addr {
    return s.conn.LocalAddr()
}

// RemoteAddr returns the remote network address. The Addr returned is shared by all invocations of RemoteAddr, so do not modify it.
func (s *KCPConn) RemoteAddr() net.Addr {
    return s.remoteAddr
}

// SetDeadline sets the deadline associated with the listener. A zero time value disables the deadline.
func (s *KCPConn) SetDeadline(t time.Time) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.deadlineRead = t
    s.deadlineWrite = t
    return nil
}

// SetReadDeadline implements the Conn SetReadDeadline method.
func (s *KCPConn) SetReadDeadline(t time.Time) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.deadlineRead = t
    return nil
}

// SetWriteDeadline implements the Conn SetWriteDeadline method.
func (s *KCPConn) SetWriteDeadline(t time.Time) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.deadlineWrite = t
    return nil
}

// SetWindowSize set maximum window size
func (s *KCPConn) SetWindowSize(sndwnd, rcvwnd int) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.kcp.setWndSize(sndwnd, rcvwnd)
}

// SetMtu sets the maximum transmission unit
func (s *KCPConn) SetMtu(mtu int) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.kcp.setMtu(mtu)
}

// SetStreamMode toggles the stream mode on/off
func (s *KCPConn) SetStreamMode(enable bool) {
    s.mu.Lock()
    defer s.mu.Unlock()
    if enable {
        s.kcp.stream = 1
    } else {
        s.kcp.stream = 0
    }
}

// SetNoDelay calls nodelay() of kcp
func (s *KCPConn) SetNoDelay(nodelay, interval, resend, nc int) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.kcp.setNoDelay(nodelay, interval, resend, nc)
}

// SetReadBuffer sets the socket read buffer, no effect if it's accepted from Listener
func (s *KCPConn) SetReadBuffer(bytes int) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    if s.listener == nil {
        if nc, ok := s.conn.(setReadBuffer); ok {
            return nc.SetReadBuffer(bytes)
        }
    }
    return errors.New(errInvalidOperation)
}

// SetWriteBuffer sets the socket write buffer, no effect if it's accepted from Listener
func (s *KCPConn) SetWriteBuffer(bytes int) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    if s.listener == nil {
        if nc, ok := s.conn.(setWriteBuffer); ok {
            return nc.SetWriteBuffer(bytes)
        }
    }
    return errors.New(errInvalidOperation)
}

// SetKeepAlive changes per-connection NAT keepalive interval; 0 to disable, default to 10s
func (s *KCPConn) SetKeepAlive(interval int) {
    atomic.StoreInt32(&s.keepAliveInterval, int32(interval))
    s.mu.Lock()
    s.keepAliveTimer = time.NewTimer(s.getKeepAliveInterval())
    s.mu.Unlock()
}

// GetConv gets conversation id of a session
func (s *KCPConn) GetConv() uint32 {
    return s.kcp.conv
}

func (s *KCPConn) notifyReadEvent() {
    select {
    case s.chReadEvent <- struct{}{}:
    default:
    }
}

func (s *KCPConn) notifyWriteEvent() {
    select {
    case s.chWriteEvent <- struct{}{}:
    default:
    }
}

func (s *KCPConn) notifyFlushEvent() {
    select {
    case s.chKcpFlushEvent <- struct{}{}:
    default:
    }
}

func (s *KCPConn) kcpOutput(buf []byte, size int) {
    //log.Printf("conv: %d kcpOutput called", s.GetConv())
    //role := "server"
    //if s.isClient {
    //    role = "client"
    //}
    //s.kcp.stats.AliveTime = s.kcp.stats.EndTime - s.kcp.stats.StartTime
    //log.Printf("-- role=%s (%p) conv=%d -- stats: %+v, isLocalOpen=%v, isRemoteOpen=%v, snd_nxt=%d, WaitSnd=%d, rcv_nxt=%d, rcv_queue=%d, rcv_buf=%d\n",
    //    role, s, s.GetConv(),s.kcp.stats, !s.kcp.isStateLocalClosed(), s.kcp.isRemoteOpen(), s.kcp.snd_nxt, s.kcp.waitSnd(), s.kcp.rcv_nxt, len(s.kcp.rcv_queue), len(s.kcp.rcv_buf))
    //log.Println()

    s.kcp.stats.EndTime = currentTickMs()
    _, err := s.conn.WriteTo(buf[:size], s.remoteAddr)

    //mutex already locked
    if err == nil {
        s.kcp.stats.PacketOut += 1
        s.kcp.stats.ByteOut += int64(size)
        atomic.AddInt64(&Stats.PacketOut, 1)
        atomic.AddInt64(&Stats.ByteOut, int64(size))
    } else {
        //FIXME: what to do?
        s.kcp.stats.ErrorOutput += 1
        atomic.AddInt64(&Stats.ErrorOutput, 1)
    }
}

type (
    // Listener defines a server listening for connections
    Listener struct {
        conn        net.PacketConn
        sessions    map[string]*KCPConn
        chAccepts   chan *KCPConn
        chDeadConns chan *KCPConn
        headerSize  int
        die         chan struct{}
        rd          atomic.Value
        wd          atomic.Value
    }

    packet struct {
        from net.Addr
        data []byte
    }
)

// monitor incoming data for all connections of server
func (l *Listener) server() {
    chPacket := make(chan packet, rxQueueLimit)

    go func() {
        for {
            data := udpPacketPool.Get().([]byte)[:udpPacketSizeLimit]
            if n, from, err := l.conn.ReadFrom(data); err == nil {
                chPacket <- packet{from, data[:n]}
            } else if err != nil {
                //FIXME: what to do ?
                log.Printf("server udp read error: %v\n", err)
                atomic.AddInt64(&Stats.ErrorRead, 1)
                close(chPacket)
                return
            }
        }
    }()

loop:
    for {
        select {
        case p := <-chPacket:
            data := p.data
            from := p.from

            if len(data) < IKCP_OVERHEAD {
                continue
            }

            addrString := from.String()

            var conv uint32
            conv = binary.LittleEndian.Uint32(data)
            conn, ok := l.sessions[addrString]
            if ok {
                if conn.GetConv() != conv {
                    if !conn.kcp.isRemoteOpen() {
                        //the existing conv is closing, and can be replace by the new conv
                        delete(l.sessions, addrString)
                        ok = false
                    } else {
                        //existing is still alive, the new conv should be ignroed
                        continue
                    }
                } else {
                    //the same conv ...
                }
            }

            if !ok {
                // if the first cmd is not connect cmd, we ignore it
                var cmd uint8
                cmd = data[4]
                if cmd != IKCP_CMD_CONNECT {
                    udpPacketPool.Put(data)
                    continue
                }
                // new session
                conn = newKCPConn()
                conn.accept(conv, l, l.conn, from)
                l.sessions[addrString] = conn
                l.chAccepts <- conn
            }

            conn.chUdpInput <- data

        case deadConn := <-l.chDeadConns:
            addrString := deadConn.remoteAddr.String()
            if conn, ok := l.sessions[addrString]; ok {
                if conn == deadConn {
                    delete(l.sessions, addrString)
                }
            }

        case <-l.die:
            break loop
        }
    }

    //TODO: close all connections
    //.....
    l.conn.Close()
}

// SetReadBuffer sets the socket read buffer for the Listener
func (l *Listener) SetReadBuffer(bytes int) error {
    if nc, ok := l.conn.(setReadBuffer); ok {
        return nc.SetReadBuffer(bytes)
    }
    return errors.New(errInvalidOperation)
}

// SetWriteBuffer sets the socket write buffer for the Listener
func (l *Listener) SetWriteBuffer(bytes int) error {
    if nc, ok := l.conn.(setWriteBuffer); ok {
        return nc.SetWriteBuffer(bytes)
    }
    return errors.New(errInvalidOperation)
}

/*
// SetDSCP sets the 6bit DSCP field of IP header
func (l *Listener) SetDSCP(dscp int) error {
    if nc, ok := l.conn.(net.Conn); ok {
        return ipv4.NewConn(nc).SetTOS(dscp << 2)
    }
    return errors.New(errInvalidOperation)
}
*/

// Accept implements the Accept method in the Listener interface; it waits for the next call and returns a generic Conn.
func (l *Listener) Accept() (net.Conn, error) {
    return l.AcceptKCP()
}

// AcceptKCP accepts a KCP connection
func (l *Listener) AcceptKCP() (*KCPConn, error) {
    var timeout <-chan time.Time
    if tdeadline, ok := l.rd.Load().(time.Time); ok && !tdeadline.IsZero() {
        timeout = time.After(tdeadline.Sub(time.Now()))
    }

    select {
    case <-timeout:
        return nil, &errTimeout{}
    case c := <-l.chAccepts:
        return c, nil
    case <-l.die:
        return nil, errors.New(errBrokenPipe)
    }
}

// SetDeadline sets the deadline associated with the listener. A zero time value disables the deadline.
func (l *Listener) SetDeadline(t time.Time) error {
    l.SetReadDeadline(t)
    l.SetWriteDeadline(t)
    return nil
}

// SetReadDeadline implements the Conn SetReadDeadline method.
func (l *Listener) SetReadDeadline(t time.Time) error {
    l.rd.Store(t)
    return nil
}

// SetWriteDeadline implements the Conn SetWriteDeadline method.
func (l *Listener) SetWriteDeadline(t time.Time) error {
    l.wd.Store(t)
    return nil
}

// Close stops listening on the UDP address. Already Accepted connections are not closed.
func (l *Listener) Close() error {
    close(l.die)

    //TODO: wait for l.conn Closed
    return nil
}

// Addr returns the listener's network address, The Addr returned is shared by all invocations of Addr, so do not modify it.
func (l *Listener) Addr() net.Addr {
    return l.conn.LocalAddr()
}

// Listen listens for incoming KCP packets addressed to the local address laddr on the network "udp",
func Listen(laddr string) (net.Listener, error) {
    udpaddr, err := net.ResolveUDPAddr("udp", laddr)
    if err != nil {
        return nil, err
    }
    conn, err := net.ListenUDP("udp", udpaddr)
    if err != nil {
        return nil, err
    }

    return ServeConn(conn)
}

// ServeConn serves KCP protocol for a single packet connection.
func ServeConn(conn net.PacketConn) (*Listener, error) {
    l := new(Listener)
    l.conn = conn
    l.sessions = make(map[string]*KCPConn)
    l.chAccepts = make(chan *KCPConn, 4096)
    l.chDeadConns = make(chan *KCPConn, 4096)
    l.die = make(chan struct{})

    go l.server()
    return l, nil
}

// Dial connects to the remote address "raddr" on the network "udp"
func DialTimeout(network, addr string, timeout time.Duration) (*KCPConn, error) {

    var err error

    var udpAddr *net.UDPAddr
    var udpConn *net.UDPConn

    udpAddr, err = net.ResolveUDPAddr(network, addr)
    if err != nil {
        return nil, err
    }

    udpConn, err = net.DialUDP(network, nil, udpAddr)
    if err != nil {
        return nil, err
    }

    var conv uint32
    binary.Read(rand.Reader, binary.LittleEndian, &conv)
    kcpConn := newKCPConn()

    go func() {
        kcpConn.connect(conv, &ConnectedUDPConn{udpConn, udpConn}, udpAddr)
    }()

    var deadline <-chan time.Time
    if timeout != 0 {
        deadline = time.After(timeout)
    }
    select {
    case <-kcpConn.chWriteEvent:
        return kcpConn, nil
    case <-deadline:
        log.Printf("DialTimeout call Close udp local %s conv %d because connect timeout", kcpConn.conn.LocalAddr(), kcpConn.GetConv())
        kcpConn.Close()
        return nil, errors.New("timeout")
    }
}

//FIXME: consider monotonic clock
//https://github.com/davecheney/junk/tree/master/clock
func currentTickMs() uint32 {
    return uint32(time.Now().UnixNano() / int64(time.Millisecond))
}

// ConnectedUDPConn is a wrapper for net.UDPConn which converts WriteTo syscalls
// to Write syscalls that are 4 times faster on some OS'es. This should only be
// used for connections that were produced by a net.Dial* call.
type ConnectedUDPConn struct {
    *net.UDPConn
    Conn net.Conn // underlying connection if any
}

// WriteTo redirects all writes to the Write syscall, which is 4 times faster.
func (c *ConnectedUDPConn) WriteTo(b []byte, addr net.Addr) (int, error) {
    return c.Write(b)
}
