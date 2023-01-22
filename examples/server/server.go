package main

import (
	"context"
	"flag"
	"log"
	"os"
	"runtime/pprof"
	"time"

	"github.com/soypat/peasocket"
)

var (
	flagAddress     string
	flagPingMessage string
	flagPingPeriod  time.Duration
)

func main() {
	fp, _ := os.Create("server.pprof")
	err := pprof.StartCPUProfile(fp)
	if err != nil {
		log.Fatal(err)
	}
	start := time.Now()

	flag.StringVar(&flagAddress, "addr", "127.0.0.1:8080", "Address on which to listen for websocket requests")
	flag.StringVar(&flagPingMessage, "pingmessage", "abc123ABC", "Heartbeat message")
	// flag.DurationVar(&flagHeartbeatPeriod, "hbperiod", 5*time.Second, "Time between heartbeat messages")
	flag.DurationVar(&flagPingPeriod, "ping", 1*time.Second, "Time between ping messages")
	flag.Parse()
	pingMessage := []byte(flagPingMessage)
	ctx := context.Background()
	log.Println("websocket listener on ", flagAddress)
	err = peasocket.ListenAndServe(ctx, flagAddress, func(ctx context.Context, sv *peasocket.Server) {
		if err := ctx.Err(); err != nil {
			panic("context err on entry: " + err.Error())
		}
		go func() {
			backoff := peasocket.ExponentialBackoff{MaxWait: 500 * time.Millisecond}
			for ctx.Err() == nil && sv.IsConnected() {
				err := sv.HandleNextFrame()
				if err != nil {
					log.Print("error reading next frame:", err)
					backoff.Miss()
				} else {
					backoff.Hit()
				}
			}
		}()
		lastPing := time.Now()
		if time.Since(start) > 20*time.Second {
			pprof.StopCPUProfile()
		}
		backoff := peasocket.ExponentialBackoff{MaxWait: 500 * time.Millisecond}
		for {
			now := time.Now()
			if now.Sub(lastPing) > flagPingPeriod {
				backoff.Hit()
				pingctx, cancel := context.WithTimeout(ctx, flagPingPeriod)
				lastPing = now
				err := sv.Ping(pingctx, pingMessage)
				cancel()
				if err != nil {
					log.Println("ping error:", err)
					return
				}
			} else {
				backoff.Miss()
			}
		}
	})
	log.Println("server error:", err)
}
