package peasocket_test

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/soypat/peasocket"
)

func ExampleClient() {
	const message = "hello world!"
	mask := uint32(time.Now().UnixMilli())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client := peasocket.NewClient("ws://localhost:1883")
	err := client.DialHandshake(ctx, nil)
	if err != nil {
		log.Fatal("while dialing:", err)
	}
	_, err = client.Tx.WritePing(mask, []byte(message))
	if err != nil {
		log.Fatal("while pinging:", err)
	}
	// Set callbacks for logging:
	client.Rx.RxCallbacks.OnCtl = func(rx *peasocket.Rx, r io.Reader) error {
		b, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		fmt.Printf("got control frame %v with data %q\n", rx.LastReceivedHeader.String(), b)
		return nil
	}
	_, err = client.Rx.ReadNextFrame()
	if err != nil {
		log.Fatal("while reading next frame:", err)
	}
	// output:
	//
}
