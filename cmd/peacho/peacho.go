package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"time"

	"github.com/soypat/peasocket"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client := peasocket.NewClient("ws://localhost:8080", nil)
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

	for {
		msg, err := client.NextMessageReader()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || client.Err() != nil {
				log.Fatal("websocket closed:", client.Err())
			}
			// log.Println("no next message err:", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
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
