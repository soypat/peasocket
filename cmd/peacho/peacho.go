package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"time"

	"github.com/soypat/peasocket"
)

func main() {
	var wsURL string
	flag.StringVar(&wsURL, "url", "ws://localhost:8080", "Websocket server URL to echo to (required).")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client := peasocket.NewClient(wsURL, nil, nil)
	err := client.DialViaHTTPClient(ctx, nil)
	if err != nil {
		log.Fatal("while dialing:", err)
	}
	defer client.CloseWebsocket(peasocket.StatusGoingAway, "peacho finalized")
	go func() {
		for {
			err := client.ReadNextFrame()
			if client.Err() != nil {
				log.Println("connection closed, ending loop")
				return
			}
			if err != nil {
				log.Println("read next frame failed:", err)
				time.Sleep(500 * time.Millisecond)
			}
		}
	}()
	// Exponential Backoff algorithm to not saturate
	// the process with calls to NextMessageReader
	// https://en.wikipedia.org/wiki/Exponential_backoff
	exponentialBackoff := time.Duration(0)
	for {
		msg, err := client.NextMessageReader()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || client.Err() != nil {
				log.Fatal("websocket closed:", client.Err())
			}
			exponentialBackoff |= 1
			exponentialBackoff <<= 1
			exponentialBackoff = minDuration(exponentialBackoff, 500*time.Millisecond)
			time.Sleep(exponentialBackoff)
			continue
		}
		exponentialBackoff = 0
		b, err := io.ReadAll(msg)
		if err != nil {
			log.Fatal("while reading message:", err)
		}
		log.Println("echoing message:", string(b))
		err = client.WriteMessage(b)
		if err != nil {
			log.Fatal("while echoing message:", err)
		}
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
