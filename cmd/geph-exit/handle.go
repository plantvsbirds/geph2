package main

import (
	"context"
	"crypto/ed25519"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/rlp"
	"github.com/geph-official/geph2/libs/cwl"
	"github.com/geph-official/geph2/libs/tinyss"
	"github.com/xtaci/smux"
	"golang.org/x/time/rate"
)

// blacklist of local networks
var cidrBlacklist []*net.IPNet

func init() {
	for _, s := range []string{
		"127.0.0.1/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
	} {
		_, n, _ := net.ParseCIDR(s)
		cidrBlacklist = append(cidrBlacklist, n)
	}
}

func isBlack(addr *net.TCPAddr) bool {
	for _, n := range cidrBlacklist {
		if n.Contains(addr.IP) {
			return true
		}
	}
	return false
}

func handle(rawClient net.Conn) {
	log.Printf("C<%p> accept", rawClient)
	defer log.Printf("C<%p> close", rawClient)
	defer rawClient.Close()
	rawClient.SetDeadline(time.Now().Add(time.Second * 30))
	tssClient, err := tinyss.Handshake(rawClient)
	if err != nil {
		log.Println("Error doing TinySS from", rawClient.RemoteAddr(), err)
		return
	}
	defer tssClient.Close()
	// copy the streams while
	var counter uint64
	// HACK: it's bridged if the remote address has a dot in it
	//isBridged := strings.Contains(rawClient.RemoteAddr().String(), ".")
	// sign the shared secret
	ssSignature := ed25519.Sign(seckey, tssClient.SharedSec())
	rlp.Encode(tssClient, &ssSignature)
	// authenticate the client
	var greeting [2][]byte
	err = rlp.Decode(tssClient, &greeting)
	if err != nil {
		log.Println("Error decoding greeting from", rawClient.RemoteAddr(), err)
		return
	}
	// create smux context
	muxSrv, err := smux.Server(tssClient, &smux.Config{
		KeepAliveInterval: time.Minute * 30,
		KeepAliveTimeout:  time.Minute * 32,
		MaxFrameSize:      32768,
		MaxReceiveBuffer:  10 * 1024 * 1024,
	})
	if err != nil {
		log.Println("Error negotiating smux from", rawClient.RemoteAddr(), err)
		return
	}
	var limiter *rate.Limiter
	err = bclient.RedeemTicket("paid", greeting[0], greeting[1])
	if err != nil {
		if onlyPaid {
			log.Printf("%v isn't paid and we only accept paid. Failing!", rawClient.RemoteAddr())
			rlp.Encode(tssClient, "FAIL")
			return
		}
		log.Printf("%v isn't paid, trying free", rawClient.RemoteAddr())
		err = bclient.RedeemTicket("free", greeting[0], greeting[1])
		if err != nil {
			log.Printf("%v isn't free either. fail", rawClient.RemoteAddr())
			rlp.Encode(tssClient, "FAIL")
			return
		}
		limiter = rate.NewLimiter(100*1000, 5*1000*1000)
		limiter.WaitN(context.Background(), 5*1000*1000-500)
	} else {
		limiter = rate.NewLimiter(rate.Inf, 10*1000*1000)
	}
	// IGNORE FOR NOW
	rlp.Encode(tssClient, "OK")
	rawClient.SetDeadline(time.Time{})
	defer muxSrv.Close()
	for {
		soxclient, err := muxSrv.AcceptStream()
		if err != nil {
			return
		}
		go func() {
			defer soxclient.Close()
			var command []string
			err = rlp.Decode(&io.LimitedReader{R: soxclient, N: 1000}, &command)
			if err != nil {
				return
			}
			if len(command) == 0 {
				return
			}
			// match command
			switch command[0] {
			case "proxy":
				if len(command) < 1 {
					return
				}
				rlp.Encode(soxclient, true)
				dialStart := time.Now()
				host := command[1]
				tcpAddr, err := net.ResolveTCPAddr("tcp4", host)
				if err != nil || isBlack(tcpAddr) {
					return
				}
				remote, err := net.DialTimeout("tcp", tcpAddr.String(), time.Second*30)
				if err != nil {
					return
				}
				// measure dial latency
				dialLatency := time.Since(dialStart)
				log.Printf("dialed %v in %.1fms", host, dialLatency.Seconds()*1000)
				if statClient != nil {
					statClient.Timing(hostname+".dialLatency", dialLatency.Milliseconds())
					statClient.Increment(hostname + ".totalConns")
					defer func() {
						statClient.Timing(hostname+".connLifetime", dialLatency.Milliseconds())
					}()
				}

				remote.SetDeadline(time.Now().Add(time.Hour))
				defer remote.Close()
				onPacket := func(l int) {
					if statClient != nil {
						before := atomic.LoadUint64(&counter)
						atomic.AddUint64(&counter, uint64(l))
						after := atomic.LoadUint64(&counter)
						if before/1000000 != after/1000000 {
							statClient.Increment(hostname + ".transferMB")
						}
					}
				}
				go func() {
					defer remote.Close()
					defer soxclient.Close()
					cwl.CopyWithLimit(remote, soxclient, limiter, onPacket)
				}()
				cwl.CopyWithLimit(soxclient, remote, limiter, onPacket)
			case "ip":
				var ip string
				if ipi, ok := ipcache.Get("ip"); ok {
					ip = ipi.(string)
				} else {
					addr := "http://checkip.amazonaws.com"
					resp, err := http.Get(addr)
					if err != nil {
						return
					}
					defer resp.Body.Close()
					ipb, err := ioutil.ReadAll(resp.Body)
					if err != nil {
						return
					}
					ip = string(ipb)
				}
				rlp.Encode(soxclient, true)
				rlp.Encode(soxclient, ip)
			}
		}()
	}
}
