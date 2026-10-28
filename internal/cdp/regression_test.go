package cdp

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSendBoundsWritesAndQueue(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	cli := &Client{conn: left, writeGate: make(chan struct{}, 1), pending: map[int]chan response{}, closed: make(chan struct{})}
	defer cli.Close()
	// Hold the writer to prove the second deadline also covers queueing.
	cli.writeGate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := cli.SendContext(ctx, "queued", nil, time.Second)
	if err == nil || time.Since(start) > 300*time.Millisecond {
		t.Fatal("writer queue ignored deadline")
	}
	<-cli.writeGate
	start = time.Now()
	_, err = cli.Send("large", map[string]string{"data": strings.Repeat("x", 1<<20)}, 30*time.Millisecond)
	if err == nil || time.Since(start) > 300*time.Millisecond {
		t.Fatal("blocked socket write ignored deadline")
	}
}
