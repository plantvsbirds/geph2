package kcp

import (
	"encoding/binary"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/patrickmn/go-cache"
)

const (
	IKCP_RTO_NDL     = 30  // no delay min rto
	IKCP_RTO_MIN     = 100 // normal min rto
	IKCP_RTO_DEF     = 200
	IKCP_RTO_MAX     = 60000
	IKCP_CMD_PUSH    = 81 // cmd: push data
	IKCP_CMD_ACK     = 82 // cmd: ack
	IKCP_CMD_WASK    = 83 // cmd: window probe (ask)
	IKCP_CMD_WINS    = 84 // cmd: window size (tell)
	IKCP_ASK_SEND    = 1  // need to send IKCP_CMD_WASK
	IKCP_ASK_TELL    = 2  // need to send IKCP_CMD_WINS
	IKCP_WND_SND     = 32
	IKCP_WND_RCV     = 32
	IKCP_MTU_DEF     = 1400
	IKCP_ACK_FAST    = 3
	IKCP_INTERVAL    = 100
	IKCP_OVERHEAD    = 24
	IKCP_DEADLINK    = 20
	IKCP_THRESH_INIT = 2
	IKCP_THRESH_MIN  = 2
	IKCP_PROBE_INIT  = 7000   // 7 secs to probe window size
	IKCP_PROBE_LIMIT = 120000 // up to 120 secs to probe window
)

// 10 seconds
const quiescentMax = 200

var CongestionControl = "LOL"

var doLogging = false

func init() {
	doLogging = os.Getenv("KCPLOG") != ""
}

// monotonic reference time point
var refTime time.Time = time.Now()

// currentMs returns current elasped monotonic milliseconds since program startup
func currentMs() uint32 { return uint32(time.Now().Sub(refTime) / time.Millisecond) }

// output_callback is a prototype which ought capture conn and call conn.Write
type output_callback func(buf []byte, size int)

/* encode 8 bits unsigned int */
func ikcp_encode8u(p []byte, c byte) []byte {
	p[0] = c
	return p[1:]
}

/* decode 8 bits unsigned int */
func ikcp_decode8u(p []byte, c *byte) []byte {
	*c = p[0]
	return p[1:]
}

/* encode 16 bits unsigned int (lsb) */
func ikcp_encode16u(p []byte, w uint16) []byte {
	binary.LittleEndian.PutUint16(p, w)
	return p[2:]
}

/* decode 16 bits unsigned int (lsb) */
func ikcp_decode16u(p []byte, w *uint16) []byte {
	*w = binary.LittleEndian.Uint16(p)
	return p[2:]
}

/* encode 32 bits unsigned int (lsb) */
func ikcp_encode32u(p []byte, l uint32) []byte {
	binary.LittleEndian.PutUint32(p, l)
	return p[4:]
}

/* decode 32 bits unsigned int (lsb) */
func ikcp_decode32u(p []byte, l *uint32) []byte {
	*l = binary.LittleEndian.Uint32(p)
	return p[4:]
}

func _imin_(a, b uint32) uint32 {
	if a <= b {
		return a
	}
	return b
}

func _imax_(a, b uint32) uint32 {
	if a >= b {
		return a
	}
	return b
}

func _ibound_(lower, middle, upper uint32) uint32 {
	return _imin_(_imax_(lower, middle), upper)
}

func _itimediff(later, earlier uint32) int32 {
	return (int32)(later - earlier)
}

// segment defines a KCP segment
type segment struct {
	conv     uint32
	cmd      uint8
	frg      uint8
	wnd      uint16
	ts       uint32
	sn       uint32
	una      uint32
	rto      uint32
	xmit     uint32
	resendts uint32
	fastack  uint32
	acked    uint32 // mark if the seg has acked
	data     []byte
}

// encode a segment into buffer
func (seg *segment) encode(ptr []byte) []byte {
	ptr = ikcp_encode32u(ptr, seg.conv)
	ptr = ikcp_encode8u(ptr, seg.cmd)
	ptr = ikcp_encode8u(ptr, seg.frg)
	ptr = ikcp_encode16u(ptr, seg.wnd)
	ptr = ikcp_encode32u(ptr, seg.ts)
	ptr = ikcp_encode32u(ptr, seg.sn)
	ptr = ikcp_encode32u(ptr, seg.una)
	ptr = ikcp_encode32u(ptr, uint32(len(seg.data)))
	atomic.AddUint64(&DefaultSnmp.OutSegs, 1)

	return ptr
}

// KCP defines a single KCP connection
type KCP struct {
	conv, mtu, mss                   uint32
	snd_una, snd_nxt, rcv_nxt        uint32
	ssthresh                         uint32
	rx_rttvar, rx_srtt               int32
	rx_rto, rx_minrto                uint32
	snd_wnd, rcv_wnd, rmt_wnd, probe uint32
	cwnd                             float64
	interval, ts_flush               uint32
	nodelay, updated                 uint32
	ts_probe, probe_wait             uint32

	isDead bool

	wmax    float64
	retrans uint64
	trans   uint64

	DRE struct {
		delivered   float64
		ppDelivered map[uint32]float64
		delTime     time.Time
		ppDelTime   map[uint32]time.Time
		avgAckRate  float64
		maxAckRate  float64
		maxAckTime  time.Time
		minRtt      float64
		minRttTime  time.Time
	}

	LOL struct {
		filledPipe   bool
		fullBwCount  int
		fullBw       float64
		lastFillTime time.Time
		gain         float64
		slack        float64
		lossRate     float64

		devi float64
	}

	fastresend     int32
	nocwnd, stream int32

	snd_queue []segment
	rcv_queue []segment
	snd_buf   []segment
	rcv_buf   []segment

	acklist []ackItem

	buffer   []byte
	reserved int
	output   output_callback

	quiescent int
}

type ackItem struct {
	sn uint32
	ts uint32
}

// NewKCP create a new kcp state machine
//
// 'conv' must be equal in the connection peers, or else data will be silently rejected.
//
// 'output' function will be called whenever these is data to be sent on wire.
func NewKCP(conv uint32, output output_callback) *KCP {
	kcp := new(KCP)
	kcp.conv = conv
	kcp.snd_wnd = IKCP_WND_SND
	kcp.rcv_wnd = IKCP_WND_RCV
	kcp.rmt_wnd = IKCP_WND_RCV
	kcp.mtu = IKCP_MTU_DEF
	kcp.mss = kcp.mtu - IKCP_OVERHEAD
	kcp.buffer = make([]byte, kcp.mtu)
	kcp.rx_rto = IKCP_RTO_DEF
	kcp.rx_minrto = IKCP_RTO_MIN
	kcp.interval = IKCP_INTERVAL
	kcp.ts_flush = IKCP_INTERVAL
	kcp.ssthresh = IKCP_THRESH_INIT
	kcp.output = output
	kcp.wmax = 1 << 30
	kcp.DRE.ppDelTime = make(map[uint32]time.Time)
	kcp.DRE.ppDelivered = make(map[uint32]float64)
	if CongestionControl == "BBR" {
		//kcp.bbrOnConnectionInit()
	}
	return kcp
}

// newSegment creates a KCP segment
func (kcp *KCP) newSegment(size int) (seg segment) {
	seg.data = xmitBuf.Get().([]byte)[:size]
	return
}

// delSegment recycles a KCP segment
func (kcp *KCP) delSegment(seg *segment) {
	if seg.data != nil {
		xmitBuf.Put(seg.data)
		seg.data = nil
	}
}

// ReserveBytes keeps n bytes untouched from the beginning of the buffer,
// the output_callback function should be aware of this.
//
// Return false if n >= mss
func (kcp *KCP) ReserveBytes(n int) bool {
	if n >= int(kcp.mtu-IKCP_OVERHEAD) || n < 0 {
		return false
	}
	kcp.reserved = n
	kcp.mss = kcp.mtu - IKCP_OVERHEAD - uint32(n)
	return true
}

// PeekSize checks the size of next message in the recv queue
func (kcp *KCP) PeekSize() (length int) {
	if len(kcp.rcv_queue) == 0 {
		return -1
	}

	seg := &kcp.rcv_queue[0]
	if seg.frg == 0 {
		return len(seg.data)
	}

	if len(kcp.rcv_queue) < int(seg.frg+1) {
		return -1
	}

	for k := range kcp.rcv_queue {
		seg := &kcp.rcv_queue[k]
		length += len(seg.data)
		if seg.frg == 0 {
			break
		}
	}
	return
}

// Receive data from kcp state machine
//
// Return number of bytes read.
//
// Return -1 when there is no readable data.
//
// Return -2 if len(buffer) is smaller than kcp.PeekSize().
func (kcp *KCP) Recv(buffer []byte) (n int) {
	peeksize := kcp.PeekSize()
	if peeksize < 0 {
		return -1
	}

	if peeksize > len(buffer) {
		return -2
	}

	var fast_recover bool
	if len(kcp.rcv_queue) >= int(kcp.rcv_wnd) {
		fast_recover = true
	}

	// merge fragment
	count := 0
	for k := range kcp.rcv_queue {
		seg := &kcp.rcv_queue[k]
		copy(buffer, seg.data)
		buffer = buffer[len(seg.data):]
		n += len(seg.data)
		count++
		kcp.delSegment(seg)
		if seg.frg == 0 {
			break
		}
	}
	if count > 0 {
		kcp.rcv_queue = kcp.remove_front(kcp.rcv_queue, count)
	}

	// move available data from rcv_buf -> rcv_queue
	count = 0
	for k := range kcp.rcv_buf {
		seg := &kcp.rcv_buf[k]
		if seg.sn == kcp.rcv_nxt && len(kcp.rcv_queue)+count < int(kcp.rcv_wnd) {
			kcp.rcv_nxt++
			count++
		} else {
			break
		}
	}

	if count > 0 {
		kcp.rcv_queue = append(kcp.rcv_queue, kcp.rcv_buf[:count]...)
		kcp.rcv_buf = kcp.remove_front(kcp.rcv_buf, count)
	}

	// fast recover
	if len(kcp.rcv_queue) < int(kcp.rcv_wnd) && fast_recover {
		// ready to send back IKCP_CMD_WINS in ikcp_flush
		// tell remote my window size
		kcp.probe |= IKCP_ASK_TELL
	}
	return
}

// Send is user/upper level send, returns below zero for error
func (kcp *KCP) Send(buffer []byte) int {
	kcp.quiescent = quiescentMax
	var count int
	if len(buffer) == 0 {
		return -1
	}
	//if kcp.nocwnd == 1 {
	//}

	// append to previous segment in streaming mode (if possible)
	if kcp.stream != 0 {
		n := len(kcp.snd_queue)
		if n > 0 {
			seg := &kcp.snd_queue[n-1]
			if len(seg.data) < int(kcp.mss) {
				capacity := int(kcp.mss) - len(seg.data)
				extend := capacity
				if len(buffer) < capacity {
					extend = len(buffer)
				}

				// grow slice, the underlying cap is guaranteed to
				// be larger than kcp.mss
				oldlen := len(seg.data)
				seg.data = seg.data[:oldlen+extend]
				copy(seg.data[oldlen:], buffer)
				buffer = buffer[extend:]
			}
		}

		if len(buffer) == 0 {
			return 0
		}
	}

	if len(buffer) <= int(kcp.mss) {
		count = 1
	} else {
		count = (len(buffer) + int(kcp.mss) - 1) / int(kcp.mss)
	}

	if count > 255 {
		return -2
	}

	if count == 0 {
		count = 1
	}

	for i := 0; i < count; i++ {
		var size int
		if len(buffer) > int(kcp.mss) {
			size = int(kcp.mss)
		} else {
			size = len(buffer)
		}
		seg := kcp.newSegment(size)
		copy(seg.data, buffer[:size])
		if kcp.stream == 0 { // message mode
			seg.frg = uint8(count - i - 1)
		} else { // stream mode
			seg.frg = 0
		}
		kcp.snd_queue = append(kcp.snd_queue, seg)
		buffer = buffer[size:]
	}
	return 0
}

func (kcp *KCP) bdp() float64 {
	return (kcp.DRE.minRtt + 20) * 0.001 * kcp.DRE.maxAckRate
}

func (kcp *KCP) update_ack(rtt int32) {
	if float64(rtt) < kcp.DRE.minRtt || time.Since(kcp.DRE.minRttTime).Seconds() > 10 {
		kcp.DRE.minRtt = float64(rtt)
		kcp.DRE.minRttTime = time.Now()
	}
	// update CWND
	// https://tools.ietf.org/html/rfc6298
	var rto uint32
	if kcp.rx_srtt == 0 {
		kcp.rx_srtt = rtt
		kcp.rx_rttvar = rtt >> 1
	} else {
		delta := rtt - kcp.rx_srtt
		kcp.rx_srtt += delta >> 3
		if delta < 0 {
			delta = -delta
		}
		if rtt < kcp.rx_srtt-kcp.rx_rttvar {
			// if the new RTT sample is below the bottom of the range of
			// what an RTT measurement is expected to be.
			// give an 8x reduced weight versus its normal weighting
			kcp.rx_rttvar += (delta - kcp.rx_rttvar) >> 5
		} else {
			kcp.rx_rttvar += (delta - kcp.rx_rttvar) >> 2
		}
	}
	rto = uint32(kcp.rx_srtt) + _imax_(kcp.interval, uint32(kcp.rx_rttvar)<<2)
	kcp.rx_rto = _ibound_(kcp.rx_minrto, rto, IKCP_RTO_MAX)
}

func (kcp *KCP) shrink_buf() {
	if len(kcp.snd_buf) > 0 {
		seg := &kcp.snd_buf[0]
		kcp.snd_una = seg.sn
	} else {
		kcp.snd_una = kcp.snd_nxt
	}
}

func (kcp *KCP) processAck(seg *segment) {
	kcp.DRE.delivered += float64(len(seg.data))
	kcp.DRE.delTime = time.Now()
	pDelivered, ok := kcp.DRE.ppDelivered[seg.sn]
	if !ok {
		return
	}
	dataAcked := kcp.DRE.delivered - pDelivered
	delete(kcp.DRE.ppDelivered, seg.sn)
	pDelTime, ok := kcp.DRE.ppDelTime[seg.sn]
	if !ok {
		return
	}
	ackElapsed := kcp.DRE.delTime.Sub(pDelTime)
	delete(kcp.DRE.ppDelTime, seg.sn)
	ackRate := dataAcked / ackElapsed.Seconds()
	appLimited := len(kcp.snd_queue) == 0
	kcp.DRE.avgAckRate = kcp.DRE.avgAckRate*0.99 + ackRate*0.01
	if kcp.DRE.maxAckRate < kcp.DRE.avgAckRate || (!appLimited && time.Since(kcp.DRE.maxAckTime).Seconds() > 10) {
		kcp.DRE.maxAckRate = kcp.DRE.avgAckRate
		kcp.DRE.maxAckTime = kcp.DRE.delTime
	}
}

func (kcp *KCP) parse_ack(sn uint32) {
	if _itimediff(sn, kcp.snd_una) < 0 || _itimediff(sn, kcp.snd_nxt) >= 0 {
		return
	}

	for k := range kcp.snd_buf {
		seg := &kcp.snd_buf[k]
		if sn == seg.sn {
			// mark and free space, but leave the segment here,
			// and wait until `una` to delete this, then we don't
			// have to shift the segments behind forward,
			// which is an expensive operation for large window
			seg.acked = 1
			kcp.processAck(seg)
			kcp.delSegment(seg)
			break
		}
		if _itimediff(sn, seg.sn) < 0 {
			break
		}
	}
}

func (kcp *KCP) parse_fastack(sn, ts uint32) {
	if _itimediff(sn, kcp.snd_una) < 0 || _itimediff(sn, kcp.snd_nxt) >= 0 {
		return
	}

	for k := range kcp.snd_buf {
		seg := &kcp.snd_buf[k]
		if _itimediff(sn, seg.sn) < 0 {
			break
		} else if sn != seg.sn && _itimediff(seg.ts, ts) <= 0 {
			seg.fastack++
		}
	}
}

func (kcp *KCP) parse_una(una uint32) {
	count := 0
	for k := range kcp.snd_buf {
		seg := &kcp.snd_buf[k]
		if _itimediff(una, seg.sn) > 0 {
			kcp.processAck(seg)
			kcp.delSegment(seg)
			count++
		} else {
			break
		}
	}
	if count > 0 {
		kcp.snd_buf = kcp.remove_front(kcp.snd_buf, count)
	}
}

// ack append
func (kcp *KCP) ack_push(sn, ts uint32) {
	kcp.quiescent = quiescentMax
	kcp.acklist = append(kcp.acklist, ackItem{sn, ts})
}

// returns true if data has repeated
func (kcp *KCP) parse_data(newseg segment) bool {
	sn := newseg.sn
	if _itimediff(sn, kcp.rcv_nxt+kcp.rcv_wnd) >= 0 ||
		_itimediff(sn, kcp.rcv_nxt) < 0 {
		return true
	}

	n := len(kcp.rcv_buf) - 1
	insert_idx := 0
	repeat := false
	for i := n; i >= 0; i-- {
		seg := &kcp.rcv_buf[i]
		if seg.sn == sn {
			repeat = true
			break
		}
		if _itimediff(sn, seg.sn) > 0 {
			insert_idx = i + 1
			break
		}
	}

	if !repeat {
		// replicate the content if it's new
		dataCopy := xmitBuf.Get().([]byte)[:len(newseg.data)]
		copy(dataCopy, newseg.data)
		newseg.data = dataCopy

		if insert_idx == n+1 {
			kcp.rcv_buf = append(kcp.rcv_buf, newseg)
		} else {
			kcp.rcv_buf = append(kcp.rcv_buf, segment{})
			copy(kcp.rcv_buf[insert_idx+1:], kcp.rcv_buf[insert_idx:])
			kcp.rcv_buf[insert_idx] = newseg
		}
	}

	// move available data from rcv_buf -> rcv_queue
	count := 0
	for k := range kcp.rcv_buf {
		seg := &kcp.rcv_buf[k]
		if seg.sn == kcp.rcv_nxt && len(kcp.rcv_queue)+count < int(kcp.rcv_wnd) {
			kcp.rcv_nxt++
			count++
		} else {
			break
		}
	}
	if count > 0 {
		kcp.rcv_queue = append(kcp.rcv_queue, kcp.rcv_buf[:count]...)
		kcp.rcv_buf = kcp.remove_front(kcp.rcv_buf, count)
	}

	return repeat
}

// Input a packet into kcp state machine.
//
// 'regular' indicates it's a real data packet from remote, and it means it's not generated from ReedSolomon
// codecs.
//
// 'ackNoDelay' will trigger immediate ACK, but surely it will not be efficient in bandwidth
func (kcp *KCP) Input(data []byte, regular, ackNoDelay bool) int {
	kcp.quiescent = quiescentMax
	snd_una := kcp.snd_una
	if len(data) < IKCP_OVERHEAD {
		return -1
	}

	var latest uint32 // the latest ack packet
	var flag int
	var inSegs uint64

	for {
		var ts, sn, length, una, conv uint32
		var wnd uint16
		var cmd, frg uint8

		if len(data) < int(IKCP_OVERHEAD) {
			break
		}

		data = ikcp_decode32u(data, &conv)
		if conv != kcp.conv {
			return -1
		}

		data = ikcp_decode8u(data, &cmd)
		data = ikcp_decode8u(data, &frg)
		data = ikcp_decode16u(data, &wnd)
		data = ikcp_decode32u(data, &ts)
		data = ikcp_decode32u(data, &sn)
		data = ikcp_decode32u(data, &una)
		data = ikcp_decode32u(data, &length)
		if len(data) < int(length) {
			return -2
		}

		if cmd != IKCP_CMD_PUSH && cmd != IKCP_CMD_ACK &&
			cmd != IKCP_CMD_WASK && cmd != IKCP_CMD_WINS {
			return -3
		}

		// only trust window updates from regular packets. i.e: latest update
		if regular {
			kcp.rmt_wnd = uint32(wnd)
		}
		kcp.parse_una(una)
		kcp.shrink_buf()

		if cmd == IKCP_CMD_ACK {
			kcp.parse_ack(sn)
			kcp.parse_fastack(sn, ts)
			flag |= 1
			latest = ts
		} else if cmd == IKCP_CMD_PUSH {
			repeat := true
			if _itimediff(sn, kcp.rcv_nxt+kcp.rcv_wnd) < 0 {
				kcp.ack_push(sn, ts)
				if _itimediff(sn, kcp.rcv_nxt) >= 0 {
					var seg segment
					seg.conv = conv
					seg.cmd = cmd
					seg.frg = frg
					seg.wnd = wnd
					seg.ts = ts
					seg.sn = sn
					seg.una = una
					seg.data = data[:length] // delayed data copying
					repeat = kcp.parse_data(seg)
				}
			}
			if regular && repeat {
				atomic.AddUint64(&DefaultSnmp.RepeatSegs, 1)
			}
		} else if cmd == IKCP_CMD_WASK {
			// ready to send back IKCP_CMD_WINS in Ikcp_flush
			// tell remote my window size
			kcp.probe |= IKCP_ASK_TELL
		} else if cmd == IKCP_CMD_WINS {
			// do nothing
		} else {
			return -3
		}

		inSegs++
		data = data[length:]
	}
	atomic.AddUint64(&DefaultSnmp.InSegs, inSegs)

	// update rtt with the latest ts
	// ignore the FEC packet
	if flag != 0 && regular {
		current := currentMs()
		if _itimediff(current, latest) >= 0 {
			kcp.update_ack(_itimediff(current, latest))
		}
	}

	// cwnd update when packet arrived
	if kcp.nocwnd == 0 {
		if acks := _itimediff(kcp.snd_una, snd_una); acks > 0 {
			kcp.trans += uint64(acks)
			switch CongestionControl {
			case "BIC":
				kcp.bic_onack(acks)
			case "LOL":
				bdp := kcp.bdp()/float64(kcp.mss) + 16
				kcp.LOL.gain = 1
				if !kcp.LOL.filledPipe {
					// check for filled pipe
					if kcp.DRE.maxAckRate > kcp.LOL.fullBw {
						// still growing
						kcp.LOL.fullBw = kcp.DRE.maxAckRate
						kcp.LOL.fullBwCount = 0
						//log.Println("growing...")
					} else {
						kcp.LOL.fullBwCount++
					}
					if kcp.LOL.fullBwCount >= 3 {
						//log.Printf("BW filled at %2.fK", kcp.LOL.fullBw/1000)
						kcp.LOL.filledPipe = true
						kcp.LOL.lastFillTime = time.Now()
						kcp.LOL.gain /= 2.89
					}
				}
				if !kcp.LOL.filledPipe {
					//log.Println("pipe not filled, pumping desired up")
					kcp.LOL.gain *= 2.89
				}
				// vibrate the gain up and down every 10 rtts
				period := int(float64(time.Now().UnixNano()) / 1e6 / kcp.DRE.minRtt)
				if period%6 == 0 {
					kcp.LOL.gain *= 1.25
				} else if period%6 == 1 {
					kcp.LOL.gain *= 0.75
				}
				targetBDP := bdp * 4
				if targetBDP > kcp.cwnd+float64(acks) {
					kcp.cwnd = (kcp.cwnd + float64(acks))
				} else {
					kcp.cwnd = (kcp.cwnd + targetBDP) / 2
				}
				if kcp.cwnd < 4 {
					kcp.cwnd = 4
				}
				if doLogging {
					log.Printf("[%p] %vK / %vK / cwnd %v / gain %.2f / %v ms %.2f%%", kcp,
						int(kcp.DRE.maxAckRate/1000),
						int(kcp.DRE.avgAckRate/1000),
						int(kcp.cwnd), kcp.LOL.gain,
						int(kcp.DRE.minRtt),
						float64(kcp.retrans)/float64(kcp.trans)*100)
				}
			}
		}
	}

	if ackNoDelay && len(kcp.acklist) > 0 { // ack immediately
		kcp.flush(true)
	}
	return 0
}

func (kcp *KCP) wnd_unused() uint16 {
	if len(kcp.rcv_queue) < int(kcp.rcv_wnd) {
		return uint16(int(kcp.rcv_wnd) - len(kcp.rcv_queue))
	}
	return 0
}

var ackDebugCache = cache.New(time.Hour, time.Hour)

// flush pending data
func (kcp *KCP) flush(ackOnly bool) uint32 {
	var busy bool
	defer func() {
		if !busy {
			kcp.quiescent--
			if kcp.quiescent <= 0 {
				kcp.quiescent = 0
				kcp.LOL.filledPipe = false
				kcp.LOL.fullBwCount = 0
				kcp.LOL.fullBw = 0
				kcp.cwnd = 4
			}
		}
	}()

	var seg segment
	seg.conv = kcp.conv
	seg.cmd = IKCP_CMD_ACK
	seg.wnd = kcp.wnd_unused()
	seg.una = kcp.rcv_nxt

	buffer := kcp.buffer
	ptr := buffer[kcp.reserved:] // keep n bytes untouched

	// makeSpace makes room for writing
	makeSpace := func(space int) {
		size := len(buffer) - len(ptr)
		if size+space > int(kcp.mtu) {
			kcp.output(buffer, size)
			ptr = buffer[kcp.reserved:]
		}
	}

	// flush bytes in buffer if there is any
	flushBuffer := func() {
		size := len(buffer) - len(ptr)
		if size > kcp.reserved {
			busy = true
			kcp.output(buffer, size)
		}
	}

	// flush acknowledges
	for i, ack := range kcp.acklist {
		busy = true
		makeSpace(IKCP_OVERHEAD)
		// filter jitters caused by bufferbloat
		if ack.sn >= kcp.rcv_nxt || len(kcp.acklist)-1 == i {
			seg.sn, seg.ts = ack.sn, ack.ts
			ptr = seg.encode(ptr)
		}
	}
	kcp.acklist = kcp.acklist[0:0]

	if ackOnly { // flash remain ack segments
		flushBuffer()
		return kcp.interval
	}

	// probe window size (if remote window size equals zero)
	if kcp.rmt_wnd == 0 {
		current := currentMs()
		if kcp.probe_wait == 0 {
			kcp.probe_wait = IKCP_PROBE_INIT
			kcp.ts_probe = current + kcp.probe_wait
		} else {
			if _itimediff(current, kcp.ts_probe) >= 0 {
				if kcp.probe_wait < IKCP_PROBE_INIT {
					kcp.probe_wait = IKCP_PROBE_INIT
				}
				kcp.probe_wait += kcp.probe_wait / 2
				if kcp.probe_wait > IKCP_PROBE_LIMIT {
					kcp.probe_wait = IKCP_PROBE_LIMIT
				}
				kcp.ts_probe = current + kcp.probe_wait
				kcp.probe |= IKCP_ASK_SEND
			}
		}
		busy = true
	} else if kcp.ts_probe != 0 || kcp.probe_wait != 0 {
		kcp.ts_probe = 0
		kcp.probe_wait = 0
		busy = true
	}

	// flush window probing commands
	if (kcp.probe & IKCP_ASK_SEND) != 0 {
		seg.cmd = IKCP_CMD_WASK
		makeSpace(IKCP_OVERHEAD)
		ptr = seg.encode(ptr)
		busy = true
	}

	// flush window probing commands
	if (kcp.probe & IKCP_ASK_TELL) != 0 {
		seg.cmd = IKCP_CMD_WINS
		makeSpace(IKCP_OVERHEAD)
		ptr = seg.encode(ptr)
		busy = true
	}

	if kcp.probe != 0 {
		kcp.probe = 0
		busy = true
	}

	// calculate window size
	cwnd := _imin_(kcp.snd_wnd, kcp.rmt_wnd)
	if kcp.nocwnd == 0 {
		cwnd = _imin_(uint32(kcp.cwnd), cwnd)
	}

	// sliding window, controlled by snd_nxt && sna_una+cwnd
	newSegsCount := 0
	for k := range kcp.snd_queue {
		busy = true
		if _itimediff(kcp.snd_nxt, kcp.snd_una+cwnd) >= 0 {
			break
		}
		newseg := kcp.snd_queue[k]
		newseg.conv = kcp.conv
		newseg.cmd = IKCP_CMD_PUSH
		newseg.sn = kcp.snd_nxt
		kcp.DRE.ppDelivered[newseg.sn] = kcp.DRE.delivered
		kcp.DRE.ppDelTime[newseg.sn] = kcp.DRE.delTime
		kcp.snd_buf = append(kcp.snd_buf, newseg)
		kcp.snd_nxt++
		newSegsCount++
	}
	if newSegsCount > 0 {
		busy = true
		kcp.snd_queue = kcp.remove_front(kcp.snd_queue, newSegsCount)
	}

	// calculate resent
	resent := uint32(kcp.fastresend)
	if kcp.fastresend <= 0 {
		resent = 0xffffffff
	}

	// check for retransmissions
	current := currentMs()
	var change, lost, lostSegs, fastRetransSegs, earlyRetransSegs uint64
	minrto := int32(kcp.interval)

	ref := kcp.snd_buf[:len(kcp.snd_buf)] // for bounds check elimination
	for k := range ref {
		busy = true
		segment := &ref[k]
		needsend := false
		if segment.acked == 1 {
			continue
		}
		const RTOSLACK = 500
		if segment.xmit == 0 { // initial transmit
			needsend = true
			segment.rto = kcp.rx_rto
			segment.resendts = current + segment.rto + RTOSLACK
			kcp.trans++
		} else if segment.fastack >= resent { // fast retransmit
			needsend = true
			segment.fastack = 0
			segment.rto = kcp.rx_rto
			segment.resendts = current + segment.rto
			change++
			fastRetransSegs++
		} else if segment.fastack > 0 && newSegsCount == 0 { // early retransmit
			needsend = true
			segment.fastack = 0
			segment.rto = kcp.rx_rto
			segment.resendts = current + segment.rto
			change++
			earlyRetransSegs++
		} else if _itimediff(current, segment.resendts) >= 0 { // RTO
			needsend = true
			// if kcp.nodelay == 0 {
			// 	segment.rto += kcp.rx_rto
			// } else {
			// 	segment.rto += kcp.rx_rto / 2
			// }
			segment.rto *= 2
			segment.fastack = 0
			segment.resendts = current + segment.rto
			if segment.rto > IKCP_RTO_MAX {
				if doLogging {
					log.Printf("[%p] self-destruct due to far too long RTO", kcp)
				}
				kcp.isDead = true
			}
			lost++
			lostSegs++
		}

		if needsend {
			current = currentMs()
			segment.xmit++
			segment.ts = current
			segment.wnd = seg.wnd
			segment.una = seg.una

			need := IKCP_OVERHEAD + len(segment.data)
			makeSpace(need)
			ptr = segment.encode(ptr)
			copy(ptr, segment.data)
			ptr = ptr[len(segment.data):]
		}

		// get the nearest rto
		if rto := _itimediff(segment.resendts, current); rto > 0 && rto < minrto {
			minrto = rto
		}
	}

	// flash remain segments
	flushBuffer()

	// counter updates
	sum := lostSegs
	if lostSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.LostSegs, lostSegs)
	}
	if fastRetransSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.FastRetransSegs, fastRetransSegs)
		sum += fastRetransSegs
	}
	if earlyRetransSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.EarlyRetransSegs, earlyRetransSegs)
		sum += earlyRetransSegs
	}
	if sum > 0 {
		atomic.AddUint64(&DefaultSnmp.RetransSegs, sum)
		kcp.retrans += sum
	}

	// cwnd update
	if kcp.nocwnd == 0 {
		switch CongestionControl {
		case "BIC":
			// congestion control, https://tools.ietf.org/html/rfc5681
			if lostSegs > 10 || change > 20 {
				kcp.bic_onloss()
			}
		case "LOL":
			// if lostSegs > 0 {
			// 	log.Println("RTO timeout, cutting cwnd to 1")
			// 	kcp.cwnd = 1
			// }
		}
		if kcp.cwnd < 32 {
			kcp.cwnd = 32
		}
	}

	return uint32(minrto)
}

// SetMtu changes MTU size, default is 1400
func (kcp *KCP) SetMtu(mtu int) int {
	if mtu < 50 || mtu < IKCP_OVERHEAD {
		return -1
	}
	if kcp.reserved >= int(kcp.mtu-IKCP_OVERHEAD) || kcp.reserved < 0 {
		return -1
	}

	buffer := make([]byte, mtu)
	if buffer == nil {
		return -2
	}
	kcp.mtu = uint32(mtu)
	kcp.mss = kcp.mtu - IKCP_OVERHEAD - uint32(kcp.reserved)
	kcp.buffer = buffer
	return 0
}

// NoDelay options
// fastest: ikcp_nodelay(kcp, 1, 20, 2, 1)
// nodelay: 0:disable(default), 1:enable
// interval: internal update timer interval in millisec, default is 100ms
// resend: 0:disable fast resend(default), 1:enable fast resend
// nc: 0:normal congestion control(default), 1:disable congestion control
func (kcp *KCP) NoDelay(nodelay, interval, resend, nc int) int {
	if nodelay >= 0 {
		kcp.nodelay = uint32(nodelay)
		if nodelay != 0 {
			kcp.rx_minrto = IKCP_RTO_NDL
		} else {
			kcp.rx_minrto = IKCP_RTO_MIN
		}
	}
	if interval >= 0 {
		if interval > 5000 {
			interval = 5000
		} else if interval < 10 {
			interval = 10
		}
		kcp.interval = uint32(interval)
	}
	if resend >= 0 {
		kcp.fastresend = int32(resend)
	}
	if nc >= 0 {
		kcp.nocwnd = int32(nc)
	}
	return 0
}

// WndSize sets maximum window size: sndwnd=32, rcvwnd=32 by default
func (kcp *KCP) WndSize(sndwnd, rcvwnd int) int {
	if sndwnd > 0 {
		kcp.snd_wnd = uint32(sndwnd)
	}
	if rcvwnd > 0 {
		kcp.rcv_wnd = uint32(rcvwnd)
	}
	return 0
}

// WaitSnd gets how many packet is waiting to be sent
func (kcp *KCP) WaitSnd() int {
	return len(kcp.snd_buf) + len(kcp.snd_queue)
}

// remove front n elements from queue
// if the number of elements to remove is more than half of the size.
// just shift the rear elements to front, otherwise just reslice q to q[n:]
// then the cost of runtime.growslice can always be less than n/2
func (kcp *KCP) remove_front(q []segment, n int) []segment {
	if n > cap(q)/2 {
		newn := copy(q, q[n:])
		return q[:newn]
	}
	return q[n:]
}
