package sip

import (
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/voice"
	"github.com/pion/rtp"
)

// MediaSession carries one call's RTP (PCMU/8000). Sequence numbers and
// timestamps must advance per packet or strict RTP stacks (baresip /
// YakPhone) discard every packet as a duplicate — the earlier constant-0
// bug produced silent calls.
type MediaSession struct {
	conn             *net.UDPConn
	remoteAddr       *net.UDPAddr
	ssrc             uint32
	payloadType      uint8
	mu               sync.Mutex
	bridge           *voice.Bridge
	closed           bool
	seq              uint16
	ts               uint32
	rx               int64
	tx               int64
	sourceCandidate  *net.UDPAddr
	candidateSSRC    uint32
	candidateSeq     uint16
	candidatePackets int
	candidateAt      time.Time
}

func NewMediaSession(localAddr string) (*MediaSession, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", localAddr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	return &MediaSession{conn: conn, ssrc: 0x12345678, payloadType: 0}, nil
}

func (m *MediaSession) LocalAddr() *net.UDPAddr { return m.conn.LocalAddr().(*net.UDPAddr) }

func (m *MediaSession) RemoteAddr() *net.UDPAddr {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.remoteAddr
}

func (m *MediaSession) SetRemote(addr string) error {
	ra, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.remoteAddr = ra
	m.sourceCandidate = nil
	m.candidatePackets = 0
	m.mu.Unlock()
	return nil
}

func (m *MediaSession) WritePCMU(frame []byte) error {
	m.mu.Lock()
	ra := m.remoteAddr
	m.seq++
	m.ts += uint32(len(frame))
	seq, ts, pt, ssrc := m.seq, m.ts, m.payloadType, m.ssrc
	m.mu.Unlock()
	if ra == nil {
		return nil
	}
	pkt := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: pt, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc},
		Payload: frame,
	}
	buf, err := pkt.Marshal()
	if err != nil {
		return err
	}
	_, err = m.conn.WriteToUDP(buf, ra)
	if err == nil {
		m.mu.Lock()
		m.tx++
		m.mu.Unlock()
	}
	return err
}

func (m *MediaSession) OnPCMUFrame(fn func([]byte)) {
	go func() {
		buf := make([]byte, 2048)
		for {
			n, remote, err := m.conn.ReadFromUDP(buf)
			if err != nil {
				m.mu.Lock()
				closed := m.closed
				m.mu.Unlock()
				if closed {
					return
				}
				continue
			}
			pkt := &rtp.Packet{}
			if err := pkt.Unmarshal(buf[:n]); err != nil {
				continue
			}
			if !m.acceptPCMU(pkt, remote) {
				continue
			}
			fn(pkt.Payload)
		}
	}()
}

// acceptPCMU uses the authenticated SIP peer's IP as the trust boundary.
// A UDP proxy may rewrite the media source port while leaving SDP unchanged.
// Confirm a short sequence from that IP, then return audio to the observed
// mapping. Unrelated peers, RTCP, other codecs and stray packets cannot move it.
func (m *MediaSession) acceptPCMU(pkt *rtp.Packet, remote *net.UDPAddr) bool {
	if pkt.Version != 2 || pkt.PayloadType != 0 || len(pkt.Payload) != voice.FrameSamples || remote == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.remoteAddr == nil || !remote.IP.Equal(m.remoteAddr.IP) || remote.Zone != m.remoteAddr.Zone {
		return false
	}
	m.rx++
	if remote.Port == m.remoteAddr.Port {
		m.sourceCandidate = nil
		m.candidatePackets = 0
		return true
	}
	now := time.Now()
	if m.sourceCandidate != nil && remote.Port == m.sourceCandidate.Port && pkt.SSRC == m.candidateSSRC && pkt.SequenceNumber == m.candidateSeq+1 && now.Sub(m.candidateAt) <= time.Second {
		m.candidatePackets++
	} else {
		m.sourceCandidate = &net.UDPAddr{IP: append(net.IP(nil), remote.IP...), Port: remote.Port, Zone: remote.Zone}
		m.candidatePackets = 1
	}
	m.candidateSSRC, m.candidateSeq, m.candidateAt = pkt.SSRC, pkt.SequenceNumber, now
	if m.candidatePackets >= 3 {
		previous := m.remoteAddr
		m.remoteAddr = m.sourceCandidate
		m.sourceCandidate, m.candidatePackets = nil, 0
		slog.Info("sip RTP return address learned", "local", m.LocalAddr().String(), "previous", previous.String(), "remote", m.remoteAddr.String())
	}
	return true
}

// Stats reports RTP counters for §36/§37 audio diagnostics.
func (m *MediaSession) Stats() (rx, tx int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rx, m.tx
}

func (m *MediaSession) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return m.conn.Close()
}
