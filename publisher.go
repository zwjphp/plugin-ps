package ps

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"time"

	"github.com/pion/rtp"
	"github.com/yapingcat/gomedia/go-mpeg2"
	"go.uber.org/zap"
	. "m7s.live/engine/v4"
	"m7s.live/engine/v4/codec"
	"m7s.live/engine/v4/codec/mpegts"
	. "m7s.live/engine/v4/track"
	"m7s.live/engine/v4/util"
	"m7s.live/plugin/ps/v4/mpegps"
)

type cacheItem struct {
	Seq uint16
	*util.ListItem[util.Buffer]
}

type PSPublisher struct {
	Publisher
	relayTrack     *PSTrack
	rtp.Packet     `json:"-" yaml:"-"`
	DisableReorder bool //是否禁用rtp重排序,TCP模式下应当禁用
	// mpegps.MpegPsStream `json:"-" yaml:"-"`
	// *mpegps.PSDemuxer `json:"-" yaml:"-"`
	mpegps.DecPSPackage `json:"-" yaml:"-"`
	reorder             util.RTPReorder[*cacheItem]
	pool                util.BytesPool
	lastSeq             uint16
	lastReceive         time.Time
	dump                *os.File
	dumpLen             []byte
}

func (p *PSPublisher) OnEvent(event any) {
	switch event.(type) {
	case IPublisher:
		p.dumpLen = make([]byte, 6)
		if conf.RelayMode != 0 {
			p.relayTrack = NewPSTrack(p.Stream)
		}
	case SEclose, SEKick:
		conf.streams.Delete(p.Stream.Path)
	}
	p.Publisher.OnEvent(event)
}

func (p *PSPublisher) ServeTCP(conn net.Conn) {
	var err error
	ps := make(util.Buffer, 1024)
	p.SetIO(conn)
	defer p.Stop()
	tcpAddr := zap.String("tcp", conn.LocalAddr().String())
	p.Info("start receive ps stream from", tcpAddr)
	defer p.Info("stop receive ps stream from", tcpAddr)
	for err == nil {
		if _, err = io.ReadFull(conn, p.dumpLen[:2]); err != nil {
			return
		}
		ps.Relloc(int(binary.BigEndian.Uint16(p.dumpLen[:2])))
		if _, err = io.ReadFull(conn, ps); err != nil {
			return
		}
		p.PushPS(ps)
	}
}

func (p *PSPublisher) ServeUDP(conn *net.UDPConn) {
	p.SetIO(conn)
	defer p.Stop()
	bufUDP := make([]byte, 1024*1024)
	udpAddr := zap.String("udp", conn.LocalAddr().String())
	p.Info("start receive ps stream from", udpAddr)
	defer p.Info("stop receive ps stream from", udpAddr)
	for {
		conn.SetReadDeadline(time.Now().Add(time.Second * 10))
		n, _, err := conn.ReadFromUDP(bufUDP)
		if err != nil {
			return
		}
		p.PushPS(bufUDP[:n])
	}
}

func (p *PSPublisher) PushPS(ps util.Buffer) {
	if err := p.Unmarshal(ps); err != nil {
		p.Error("gb28181 decode rtp error:", zap.Error(err))
	} else if !p.IsClosed() {
		p.writeDump(ps)
	}
	p.pushPS()
}

// 解析rtp封装 https://www.ietf.org/rfc/rfc2250.txt
func (p *PSPublisher) pushPS() {
	if p.Stream == nil {
		return
	}
	if p.pool == nil {
		// p.PSDemuxer = mpegps.NewPSDemuxer()
		// p.PSDemuxer.OnPacket = p.OnPacket
		// p.PSDemuxer.OnFrame = p.OnFrame
		p.EsHandler = p
		p.lastSeq = p.SequenceNumber - 1
		p.pool = make(util.BytesPool, 17)
	}
	if conf.RelayMode != 0 {
		item := p.pool.Get(len(p.Packet.Payload))
		copy(item.Value, p.Packet.Payload)
		p.relayTrack.Push(item)
	}
	if conf.RelayMode == 1 && p.relayTrack.PSM != nil {
		return
	}
	if p.DisableReorder {
		p.Feed(p.Packet.Payload)
		p.lastSeq = p.SequenceNumber
	} else {
		item := p.pool.Get(len(p.Packet.Payload))
		copy(item.Value, p.Packet.Payload)
		for rtpPacket := p.reorder.Push(p.SequenceNumber, &cacheItem{p.SequenceNumber, item}); rtpPacket != nil; rtpPacket = p.reorder.Pop() {
			if rtpPacket.Seq != p.lastSeq+1 {
				p.Debug("drop", zap.Uint16("seq", rtpPacket.Seq), zap.Uint16("lastSeq", p.lastSeq))
				p.Reset()
				if p.VideoTrack != nil {
					p.SetLostFlag()
				}
			}
			p.Feed(rtpPacket.Value)
			p.lastSeq = rtpPacket.Seq
			rtpPacket.Recycle()
		}
	}
}
func (p *PSPublisher) OnFrame(frame []byte, cid mpeg2.PS_STREAM_TYPE, pts uint64, dts uint64) {
	switch cid {
	case mpeg2.PS_STREAM_AAC:
		if p.AudioTrack != nil {
			p.AudioTrack.WriteADTS(uint32(pts), util.ReuseBuffer{frame})
		} else {
			p.AudioTrack = NewAAC(p.Publisher.Stream, p.pool)
		}
	case mpeg2.PS_STREAM_G711A:
		if p.AudioTrack != nil {
			p.AudioTrack.WriteRawBytes(uint32(pts), util.ReuseBuffer{frame})
		} else {
			p.AudioTrack = NewG711(p.Publisher.Stream, true, p.pool)
		}
	case mpeg2.PS_STREAM_G711U:
		if p.AudioTrack != nil {
			p.AudioTrack.WriteRawBytes(uint32(pts), util.ReuseBuffer{frame})
		} else {
			p.AudioTrack = NewG711(p.Publisher.Stream, false, p.pool)
		}
	case mpeg2.PS_STREAM_H264:
		if p.VideoTrack != nil {
			// p.WriteNalu(uint32(pts), uint32(dts), frame)
			p.WriteAnnexB(uint32(pts), uint32(dts), frame)
		} else {
			p.VideoTrack = NewH264(p.Publisher.Stream, p.pool)
		}
	case mpeg2.PS_STREAM_H265:
		if p.VideoTrack != nil {
			// p.WriteNalu(uint32(pts), uint32(dts), frame)
			p.WriteAnnexB(uint32(pts), uint32(dts), frame)
		} else {
			p.VideoTrack = NewH265(p.Publisher.Stream, p.pool)
		}
	}
}

func (p *PSPublisher) OnPacket(pkg mpeg2.Display, decodeResult error) {
	// switch value := pkg.(type) {
	// case *mpeg2.PSPackHeader:
	// 	// fd3.WriteString("--------------PS Pack Header--------------\n")
	// 	if decodeResult == nil {
	// 		// value.PrettyPrint(fd3)
	// 	} else {
	// 		// fd3.WriteString(fmt.Sprintf("Decode Ps Packet Failed %s\n", decodeResult.Error()))
	// 	}
	// case *mpeg2.System_header:
	// 	// fd3.WriteString("--------------System Header--------------\n")
	// 	if decodeResult == nil {
	// 		// value.PrettyPrint(fd3)
	// 	} else {
	// 		// fd3.WriteString(fmt.Sprintf("Decode Ps Packet Failed %s\n", decodeResult.Error()))
	// 	}
	// case *mpeg2.Program_stream_map:
	// 	// fd3.WriteString("--------------------PSM-------------------\n")
	// 	if decodeResult == nil {
	// 		// value.PrettyPrint(fd3)
	// 	} else {
	// 		// fd3.WriteString(fmt.Sprintf("Decode Ps Packet Failed %s\n", decodeResult.Error()))
	// 	}
	// case *mpeg2.PesPacket:
	// 	// fd3.WriteString("-------------------PES--------------------\n")
	// 	if decodeResult == nil {
	// 		// value.PrettyPrint(fd3)
	// 	} else {
	// 		// fd3.WriteString(fmt.Sprintf("Decode Ps Packet Failed %s\n", decodeResult.Error()))
	// 	}
	// }
}

func (p *PSPublisher) ReceiveVideo(es mpegps.MpegPsEsStream) {
	if !conf.PubVideo || conf.RelayMode == 1 {
		return
	}
	if p.VideoTrack == nil {
		switch es.Type {
		case mpegts.STREAM_TYPE_H264:
			p.VideoTrack = NewH264(p.Publisher.Stream, p.pool)
		case mpegts.STREAM_TYPE_H265:
			p.VideoTrack = NewH265(p.Publisher.Stream, p.pool)
		default:
			//推测编码类型
			var maybe264 codec.H264NALUType
			maybe264 = maybe264.Parse(es.Buffer[4])
			switch maybe264 {
			case codec.NALU_Non_IDR_Picture,
				codec.NALU_IDR_Picture,
				codec.NALU_SEI,
				codec.NALU_SPS,
				codec.NALU_PPS,
				codec.NALU_Access_Unit_Delimiter:
				p.VideoTrack = NewH264(p.Publisher.Stream, p.pool)
			default:
				p.Info("maybe h265", zap.Uint8("type", maybe264.Byte()))
				p.VideoTrack = NewH265(p.Publisher.Stream, p.pool)
			}
		}
	}
	payload, pts, dts := es.Buffer, es.PTS, es.DTS
	if dts == 0 {
		dts = pts
	}
	// if binary.BigEndian.Uint32(payload) != 1 {
	// 	panic("not annexb")
	// }
	p.WriteAnnexB(pts, dts, payload)
}

func (p *PSPublisher) ReceiveAudio(es mpegps.MpegPsEsStream) {
	if !conf.PubAudio || conf.RelayMode == 1 {
		return
	}
	ts, payload := es.PTS, es.Buffer
	if p.AudioTrack == nil {
		switch es.Type {
		case mpegts.STREAM_TYPE_G711A:
			p.AudioTrack = NewG711(p.Publisher.Stream, true, p.pool)
		case mpegts.STREAM_TYPE_G711U:
			p.AudioTrack = NewG711(p.Publisher.Stream, false, p.pool)
		case mpegts.STREAM_TYPE_AAC:
			p.AudioTrack = NewAAC(p.Publisher.Stream, p.pool)
			p.WriteADTS(ts, util.ReuseBuffer{payload})
		case 0: //推测编码类型
			if payload[0] == 0xff && payload[1]>>4 == 0xf {
				p.AudioTrack = NewAAC(p.Publisher.Stream)
				p.WriteADTS(ts, util.ReuseBuffer{payload})
			}
		default:
			p.Error("audio type not supported yet", zap.Uint8("type", es.Type))
		}
	} else if es.Type == mpegts.STREAM_TYPE_AAC {
		p.WriteADTS(ts, util.ReuseBuffer{payload})
	} else {
		p.WriteRawBytes(ts, util.ReuseBuffer{payload})
	}
}
func (p *PSPublisher) writeDump(ps util.Buffer) {
	if p.dump != nil {
		util.PutBE(p.dumpLen[:4], ps.Len())
		if p.lastReceive.IsZero() {
			util.PutBE(p.dumpLen[4:], 0)
		} else {
			util.PutBE(p.dumpLen[4:], uint16(time.Since(p.lastReceive).Milliseconds()))
		}
		p.lastReceive = time.Now()
		p.dump.Write(p.dumpLen)
		p.dump.Write(ps)
	}
}
func (p *PSPublisher) Replay(f *os.File) (err error) {
	defer f.Close()
	var t uint16
	for l := make([]byte, 6); !p.IsClosed(); time.Sleep(time.Millisecond * time.Duration(t)) {
		_, err = f.Read(l)
		if err != nil {
			return
		}
		payload := make([]byte, util.ReadBE[int](l[:4]))
		t = util.ReadBE[uint16](l[4:])
		_, err = f.Read(payload)
		if err != nil {
			return
		}
		p.PushPS(payload)
	}
	return
}
func (p *PSPublisher) ReceivePSM(buf util.Buffer) {
	if p.relayTrack != nil {
		p.relayTrack.PSM = buf.Clone()
	}
}
