package main

import (
	"context"
	"flag"
	"log"
	"time"

	"github.com/soypat/peasocket"
)

var (
	flagAddress         string
	flagPingMessage     string
	flagHeartbeatPeriod time.Duration
	flagPingPeriod      time.Duration
)

func main() {
	flag.StringVar(&flagAddress, "addr", "127.0.0.1:8080", "Address on which to listen for websocket requests")
	flag.StringVar(&flagPingMessage, "pingmessage", "abc123ABC", "Heartbeat message")
	// flag.DurationVar(&flagHeartbeatPeriod, "hbperiod", 5*time.Second, "Time between heartbeat messages")
	flag.DurationVar(&flagPingPeriod, "ping", 1*time.Second, "Time between ping messages")
	flag.Parse()
	pingMessage := []byte(flagPingMessage)
	ctx := context.Background()
	log.Println("websocket listener on ", flagAddress)
	err := peasocket.ListenAndServe(ctx, flagAddress, func(ctx context.Context, s *peasocket.Server) {
		lastPing := time.Now()
		for {
			now := time.Now()
			if now.Sub(lastPing) > flagPingPeriod {
				lastPing = now
				err := s.Ping(ctx, pingMessage)
				if err != nil {
					log.Println("ping error:", err)
					return
				}
			}
		}
	})
	log.Println("server error:", err)
}
