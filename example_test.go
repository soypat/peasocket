package peasocket_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"github.com/soypat/peasocket"
)

func ExampleClient_newNotWorking() {
	const (
		message = "Hello!"
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client := peasocket.NewClient("ws://localhost:8080", nil)
	err := client.DialViaHTTPClient(ctx, nil)
	if err != nil {
		log.Fatal("while dialing:", err)
	}
	defer client.CloseWebsocket(peasocket.StatusGoingAway, "bye bye")
	log.Printf("protocol switch success. prepare msg=%q", message)
	go func() {
		for {
			err := client.ReadNextFrame()
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
		msg, err := client.NextMessageReader()
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
	//Output:
	// sdasd
}

func ExampleClient_legacy() {
	const (
		message = "Hello!"
	)
	mask := uint32(time.Now().UnixMilli())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)

	defer cancel()
	client := peasocket.NewClient("ws://localhost:8080", nil)
	err := client.DialViaHTTPClient(ctx, nil)
	if err != nil {
		log.Fatal("while dialing:", err)
	}
	defer client.CloseWebsocket(peasocket.StatusGoingAway, "bye bye")
	log.Printf("protocol switch success. prepare msg=%q with mask %#X", message, mask)

	// Set callbacks for logging:
	client.Rx.RxCallbacks.OnCtl = func(rx *peasocket.Rx, r io.Reader) error {
		b, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if rx.LastReceivedHeader.FrameType() == peasocket.FramePing {
			log.Printf("received ping, replying with pong:%q", b)
			_, err = client.Tx.WritePong(b)
			log.Printf("pong replied: %v", err)
			return err
		}
		if string(b) != message {
			log.Println("message does not match ping!")
		} else {
			log.Println("ping success! message matches!")
		}
		fmt.Printf("got control frame %v with data %q\n", rx.LastReceivedHeader.String(), b)
		return nil
	}

	n, err := client.Rx.ReadNextFrame()
	if err != nil {
		log.Fatal("while reading next frame:", err)
	}
	if n == 0 {
		log.Println("nothing read. sleep a bit...")
		time.Sleep(time.Second)
	}

	// output:
	// as
}
