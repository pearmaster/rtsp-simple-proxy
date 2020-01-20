package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aler9/gortsplib"
	"gortc.io/sdp"
)

const (
	_DIAL_TIMEOUT          = 10 * time.Second
	_RETRY_INTERVAL        = 5 * time.Second
	_CHECK_STREAM_INTERVAL = 6 * time.Second
	_STREAM_DEAD_AFTER     = 5 * time.Second
	_KEEPALIVE_INTERVAL    = 10 * time.Second
	_READ_TIMEOUT          = 5 * time.Second
)

func writeReqReadRes(conn *gortsplib.Conn, req *gortsplib.Request) (*gortsplib.Response, error) {
	err := conn.WriteRequest(req)
	if err != nil {
		return nil, err
	}

	conn.NetConn().SetReadDeadline(time.Now().Add(_READ_TIMEOUT))
	return conn.ReadResponse()
}

func md5Hex(in string) string {
	h := md5.New()
	h.Write([]byte(in))
	return hex.EncodeToString(h.Sum(nil))
}

type streamUdpListenerPair struct {
	rtpl  *streamUdpListener
	rtcpl *streamUdpListener
}

type auth map[string]string

func newAuth(in string) (auth, error) {
	if !strings.HasPrefix(in, "Digest ") {
		return nil, fmt.Errorf("auth does not begin with Digest")
	}
	in = in[len("Digest "):]

	a := make(auth)
	matches := regexp.MustCompile("([a-z]+)=\"(.+?)\",?").FindAllStringSubmatch(in, -1)
	for _, match := range matches {
		a[match[1]] = match[2]
	}

	return a, nil
}

type authProvider struct {
	user  string
	pass  string
	realm string
	nonce string
}

func newAuthProvider(user string, pass string, realm string, nonce string) *authProvider {
	return &authProvider{
		user:  user,
		pass:  pass,
		realm: realm,
		nonce: nonce,
	}
}

func (ap *authProvider) generateHeader(method string, path string) string {
	ha1 := md5Hex(ap.user + ":" + ap.realm + ":" + ap.pass)
	ha2 := md5Hex(method + ":" + path)
	response := md5Hex(ha1 + ":" + ap.nonce + ":" + ha2)

	return fmt.Sprintf("Digest username=\"%s\", realm=\"%s\", nonce=\"%s\", uri=\"%s\", response=\"%s\"",
		ap.user, ap.realm, ap.nonce, path, response)
}

type streamState int

const (
	_STREAM_STATE_STARTING streamState = iota
	_STREAM_STATE_READY
)

type stream struct {
	p         *program
	state     streamState
	path      string
	conf      streamConf
	ur        *url.URL
	proto     streamProtocol
	sdpText   []byte
	sdpParsed *sdp.Message
}

func newStream(p *program, path string, conf streamConf) (*stream, error) {
	ur, err := url.Parse(conf.Url)
	if err != nil {
		return nil, err
	}

	if ur.Scheme != "rtsp" {
		return nil, fmt.Errorf("unsupported scheme: %s", ur.Scheme)
	}

	proto := _STREAM_PROTOCOL_UDP
	if conf.UseTcp {
		proto = _STREAM_PROTOCOL_TCP
	}

	s := &stream{
		p:     p,
		state: _STREAM_STATE_STARTING,
		path:  path,
		conf:  conf,
		ur:    ur,
		proto: proto,
	}

	go s.run()

	return s, nil
}

func (s *stream) log(format string, args ...interface{}) {
	format = "[STREAM " + s.path + "] " + format
	log.Printf(format, args...)
}

func (s *stream) run() {
	firstTime := true

	for {
		if firstTime {
			firstTime = false
		} else {
			time.Sleep(_RETRY_INTERVAL)
		}

		s.log("starting with protocol %s", s.proto)

		func() {
			nconn, err := net.DialTimeout("tcp", s.ur.Host, _DIAL_TIMEOUT)
			if err != nil {
				s.log("ERR: %s", err)
				return
			}
			defer nconn.Close()

			conn := gortsplib.NewConn(nconn)
			cseq := 1

			res, err := writeReqReadRes(conn, &gortsplib.Request{
				Method: "OPTIONS",
				Url:    "rtsp://" + s.ur.Host + "/",
				Headers: map[string]string{
					"CSeq": strconv.FormatInt(int64(cseq), 10),
				},
			})
			cseq += 1
			if err != nil {
				s.log("ERR: %s", err)
				return
			}

			if res.StatusCode != 200 {
				s.log("ERR: OPTIONS returned code %d", res.StatusCode)
				return
			}

			res, err = writeReqReadRes(conn, &gortsplib.Request{
				Method: "DESCRIBE",
				Url:    "rtsp://" + s.ur.Host + s.ur.Path,
				Headers: map[string]string{
					"CSeq": strconv.FormatInt(int64(cseq), 10),
				},
			})
			cseq += 1
			if err != nil {
				s.log("ERR: %s", err)
				return
			}

			var authProv *authProvider

			if res.StatusCode == 401 {
				if s.ur.User == nil {
					s.log("ERR: 401 but user not provided")
					return
				}

				user := s.ur.User.Username()
				pass, _ := s.ur.User.Password()
				if pass == "" {
					s.log("ERR: 401 but password not provided")
					return
				}

				rawAuth, ok := res.Headers["WWW-Authenticate"]
				if !ok {
					s.log("ERR: 401 but WWW-Authenticate not provided")
					return
				}

				auth, err := newAuth(rawAuth)
				if err != nil {
					s.log("ERR: %s", err)
					return
				}

				nonce, ok := auth["nonce"]
				if !ok {
					s.log("ERR: 401 but nonce not provided")
					return
				}

				realm, ok := auth["realm"]
				if !ok {
					s.log("ERR: 401 but realm not provided")
					return
				}

				authProv = newAuthProvider(user, pass, realm, nonce)

				res, err = writeReqReadRes(conn, &gortsplib.Request{
					Method: "DESCRIBE",
					Url:    "rtsp://" + s.ur.Host + s.ur.Path,
					Headers: map[string]string{
						"CSeq":          strconv.FormatInt(int64(cseq), 10),
						"Authorization": authProv.generateHeader("DESCRIBE", "rtsp://"+s.ur.Host+s.ur.Path),
					},
				})
				cseq += 1
				if err != nil {
					s.log("ERR: %s", err)
					return
				}
			}

			if res.StatusCode != 200 {
				s.log("ERR: DESCRIBE returned code %d", res.StatusCode)
				return
			}

			contentType, ok := res.Headers["Content-Type"]
			if !ok {
				s.log("ERR: Content-Type not provided")
				return
			}

			if contentType != "application/sdp" {
				s.log("ERR: wrong Content-Type, expected application/sdp")
				return
			}

			sdpParsed, err := func() (*sdp.Message, error) {
				s, err := sdp.DecodeSession(res.Content, nil)
				if err != nil {
					return nil, err
				}

				m := &sdp.Message{}
				d := sdp.NewDecoder(s)
				err = d.Decode(m)
				if err != nil {
					return nil, err
				}

				return m, nil
			}()
			if err != nil {
				s.log("ERR: invalid SDP")
				return
			}

			// remove any attribute that can mess with the proxy
			removeAttr := func(in sdp.Attributes) sdp.Attributes {
				for i := 0; i < len(in); i++ {
					attr := in[i]
					if attr.Key == "control" {
						in = append(in[:i], in[i+1:]...)
					}
				}
				return in
			}
			sdpParsed.Attributes = removeAttr(sdpParsed.Attributes)
			for _, m := range sdpParsed.Medias {
				m.Attributes = removeAttr(m.Attributes)
			}
			sdps := sdp.Session{}
			sdps = sdpParsed.Append(sdps)
			res.Content = sdps.AppendTo(nil)

			func() {
				s.p.mutex.Lock()
				defer s.p.mutex.Unlock()

				s.sdpText = res.Content
				s.sdpParsed = sdpParsed
			}()

			if s.proto == _STREAM_PROTOCOL_UDP {
				s.runUdp(conn, cseq)
			} else {
				s.runTcp(conn, cseq)
			}
		}()
	}
}

func (s *stream) runUdp(conn *gortsplib.Conn, cseq int) {
	publisherAddr, err := net.ResolveUDPAddr("udp", s.ur.Hostname()+":0")
	if err != nil {
		s.log("ERR: %s", err)
		return
	}

	var streamUdpListenerPairs []streamUdpListenerPair

	defer func() {
		for _, pair := range streamUdpListenerPairs {
			pair.rtpl.close()
			pair.rtcpl.close()
		}
	}()

	for i := 0; i < len(s.sdpParsed.Medias); i++ {
		var rtpPort int
		var rtcpPort int
		var rtpl *streamUdpListener
		var rtcpl *streamUdpListener
		err := func() error {
			for {
				// choose two consecutive ports in range 65536-10000
				// rtp must be pair and rtcp odd
				rtpPort = (rand.Intn((65535-10000)/2) * 2) + 10000
				rtcpPort = rtpPort + 1

				rtpl, err = newStreamUdpListener(s.p, rtpPort)
				if err != nil {
					continue
				}

				rtcpl, err = newStreamUdpListener(s.p, rtcpPort)
				if err != nil {
					continue
				}

				return nil
			}
		}()
		if err != nil {
			s.log("ERR: %s", err)
			return
		}

		res, err := writeReqReadRes(conn, &gortsplib.Request{
			Method: "SETUP",
			Url:    "rtsp://" + s.ur.Host + s.ur.Path + "/trackID=" + strconv.FormatInt(int64(i+1), 10),
			Headers: map[string]string{
				"CSeq":      strconv.FormatInt(int64(cseq), 10),
				"Transport": fmt.Sprintf("RTP/AVP/UDP;unicast;client_port=%d-%d", rtpPort, rtcpPort),
			},
		})
		cseq += 1
		if err != nil {
			s.log("ERR: %s", err)
			rtpl.close()
			rtcpl.close()
			return
		}

		if res.StatusCode != 200 {
			s.log("ERR: SETUP returned code %d", res.StatusCode)
			rtpl.close()
			rtcpl.close()
			return
		}

		rawTh, ok := res.Headers["Transport"]
		if !ok {
			s.log("ERR: transport header not provided")
			rtpl.close()
			rtcpl.close()
			return
		}

		th := gortsplib.NewTransportHeader(rawTh)
		rtpServerPort, rtcpServerPort := th.GetPorts("server_port")
		if rtpServerPort == 0 {
			s.log("ERR: server ports not provided")
			rtpl.close()
			rtcpl.close()
			return
		}

		rtpl.publisherIp = publisherAddr.IP
		rtpl.publisherPort = rtpServerPort
		rtpl.trackId = i
		rtpl.flow = _TRACK_FLOW_RTP
		rtpl.path = s.path

		rtcpl.publisherIp = publisherAddr.IP
		rtcpl.publisherPort = rtcpServerPort
		rtcpl.trackId = i
		rtcpl.flow = _TRACK_FLOW_RTCP
		rtcpl.path = s.path

		streamUdpListenerPairs = append(streamUdpListenerPairs, streamUdpListenerPair{
			rtpl:  rtpl,
			rtcpl: rtcpl,
		})
	}

	res, err := writeReqReadRes(conn, &gortsplib.Request{
		Method: "PLAY",
		Url:    "rtsp://" + s.ur.Host + s.ur.Path,
		Headers: map[string]string{
			"CSeq": strconv.FormatInt(int64(cseq), 10),
		},
	})
	cseq += 1
	if err != nil {
		s.log("ERR: %s", err)
		return
	}

	if res.StatusCode != 200 {
		s.log("ERR: PLAY returned code %d", res.StatusCode)
		return
	}

	for _, pair := range streamUdpListenerPairs {
		pair.rtpl.start()
		pair.rtcpl.start()
	}

	tickerSendKeepalive := time.NewTicker(_KEEPALIVE_INTERVAL)
	tickerCheckStream := time.NewTicker(_CHECK_STREAM_INTERVAL)

	func() {
		s.p.mutex.Lock()
		defer s.p.mutex.Unlock()
		s.state = _STREAM_STATE_READY
	}()

	defer func() {
		s.p.mutex.Lock()
		defer s.p.mutex.Unlock()
		s.state = _STREAM_STATE_STARTING

		// disconnect all clients
		for c := range s.p.clients {
			if c.path == s.path {
				c.close()
			}
		}
	}()

	s.log("ready")

	for {
		select {
		case <-tickerSendKeepalive.C:
			_, err = writeReqReadRes(conn, &gortsplib.Request{
				Method: "OPTIONS",
				Url:    "rtsp://" + s.ur.Host + "/",
				Headers: map[string]string{
					"CSeq": strconv.FormatInt(int64(cseq), 10),
				},
			})
			cseq += 1
			if err != nil {
				s.log("ERR: %s", err)
				return
			}

		case <-tickerCheckStream.C:
			lastFrameTime := time.Time{}

			getLastFrameTime := func(l *streamUdpListener) {
				l.mutex.Lock()
				defer l.mutex.Unlock()
				if l.lastFrameTime.After(lastFrameTime) {
					lastFrameTime = l.lastFrameTime
				}
			}

			for _, pair := range streamUdpListenerPairs {
				getLastFrameTime(pair.rtpl)
				getLastFrameTime(pair.rtcpl)
			}

			if time.Since(lastFrameTime) >= _STREAM_DEAD_AFTER {
				s.log("ERR: stream is dead")
				return
			}
		}
	}
}

func (s *stream) runTcp(conn *gortsplib.Conn, cseq int) {

	for i := 0; i < len(s.sdpParsed.Medias); i++ {
		interleaved := fmt.Sprintf("interleaved=%d-%d", (i / 2), (i/2)+1)

		res, err := writeReqReadRes(conn, &gortsplib.Request{
			Method: "SETUP",
			Url:    "rtsp://" + s.ur.Host + s.ur.Path + "/trackID=" + strconv.FormatInt(int64(i+1), 10),
			Headers: map[string]string{
				"CSeq":      strconv.FormatInt(int64(cseq), 10),
				"Transport": fmt.Sprintf("RTP/AVP/TCP;unicast;%s", interleaved),
			},
		})
		cseq += 1
		if err != nil {
			s.log("ERR: %s", err)
			return
		}

		if res.StatusCode != 200 {
			s.log("ERR: SETUP returned code %d", res.StatusCode)
			return
		}

		rawTh, ok := res.Headers["Transport"]
		if !ok {
			s.log("ERR: transport header not provided")
			return
		}

		th := gortsplib.NewTransportHeader(rawTh)

		_, ok = th[interleaved]
		if !ok {
			s.log("ERR: transport header does not have %s", interleaved)
			return
		}
	}

	res, err := writeReqReadRes(conn, &gortsplib.Request{
		Method: "PLAY",
		Url:    "rtsp://" + s.ur.Host + s.ur.Path,
		Headers: map[string]string{
			"CSeq": strconv.FormatInt(int64(cseq), 10),
		},
	})
	cseq += 1
	if err != nil {
		s.log("ERR: %s", err)
		return
	}

	if res.StatusCode != 200 {
		s.log("ERR: PLAY returned code %d", res.StatusCode)
		return
	}

	func() {
		s.p.mutex.Lock()
		defer s.p.mutex.Unlock()
		s.state = _STREAM_STATE_READY
	}()

	defer func() {
		s.p.mutex.Lock()
		defer s.p.mutex.Unlock()
		s.state = _STREAM_STATE_STARTING

		// disconnect all clients
		for c := range s.p.clients {
			if c.path == s.path {
				c.close()
			}
		}
	}()

	s.log("ready")

	buf := make([]byte, 2048)
	for {
		conn.NetConn().SetReadDeadline(time.Now().Add(_READ_TIMEOUT))
		channel, n, err := conn.ReadInterleavedFrame(buf)
		if err != nil {
			s.log("ERR: %s", err)
			return
		}

		trackId, trackFlow := interleavedChannelToTrack(channel)

		func() {
			s.p.mutex.RLock()
			defer s.p.mutex.RUnlock()

			s.p.forwardTrack(s.path, trackId, trackFlow, buf[:n])
		}()
	}
}