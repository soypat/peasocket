package peasocket_test

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"time"

	"github.com/soypat/peasocket"
)

func ExampleClient_echo() {
	const (
		message = "Hello!"
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client := peasocket.NewClient("ws://localhost:8080", nil, nil)
	err := client.DialViaHTTPClient(ctx, nil)
	if err != nil {
		log.Fatal("while dialing:", err)
	}
	defer client.CloseWebsocket(&peasocket.CloseError{
		Status: peasocket.StatusGoingAway,
		Reason: []byte("bye bye"),
	})
	log.Printf("protocol switch success. prepare msg=%q", message)
	go func() {
		// This goroutine reads frames from network.
		for {
			err := client.HandleNextFrame()
			if errors.Is(err, net.ErrClosed) {
				log.Println("connection closed, ending loop")
				return
			}
			if err != nil {
				log.Println("read next frame failed:", err)
				time.Sleep(500 * time.Millisecond)
			}
		}
	}()

	for {
		// This goroutine gets messages that have been read
		// from the client's buffer and prints them.
		msg, _, err := client.BufferedMessageReader()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				log.Fatal("websocket closed")
			}
			log.Println("no next message err:", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		b, err := io.ReadAll(msg)
		log.Println("got message:", string(b))
	}
}
