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
	var (
		wsURL     string
		keepGoing bool
	)
	flag.StringVar(&wsURL, "url", "ws://localhost:8080", "Websocket server URL to echo to (required).")
	flag.BoolVar(&keepGoing, "k", true, "Retry until program terminated")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client := peasocket.NewClient(wsURL, nil, nil)
	err := client.Dial(ctx, nil)
	if err != nil {
		log.Fatal("while dialing:", err)
	}
	defer client.CloseWebsocket(&peasocket.CloseError{
		Status: peasocket.StatusGoingAway,
		Reason: []byte("peacho finalized"),
	})
	go func() {
		for {
			err := client.ReadNextFrame()
			if client.Err() != nil {
				if keepGoing {
					log.Println("failure, retrying:", client.Err())
					client.CloseConn(client.Err())
					client.Dial(ctx, nil)
					time.Sleep(time.Second)
					continue
				}
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
	backoff := peasocket.ExponentialBackoff{MaxWait: 500 * time.Millisecond}
	for {
		msg, _, err := client.NextMessageReader()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || client.Err() != nil {
				log.Fatal("websocket closed:", client.Err())
			}
			backoff.Miss()
			continue
		}
		backoff.Hit()
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
