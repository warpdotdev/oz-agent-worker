package worker

import (
	"fmt"
	"sync"
	"time"
)

const outboundQueueSendTimeout = 5 * time.Second

// outboundQueue is a handle to the WebSocket write loop. The rest of the worker
// uses it to asynchronously send messages to the server.
type outboundQueue struct {
	messages chan []byte
	// mutex makes the closed check and channel send atomic with respect to
	// Close. An atomic boolean alone would still allow a send-after-close race.
	mutex  sync.Mutex
	closed bool
}

func newOutboundQueue(capacity int) *outboundQueue {
	return &outboundQueue{
		messages: make(chan []byte, capacity),
	}
}

func (q *outboundQueue) Send(message []byte) error {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	if q.closed {
		return fmt.Errorf("worker is shutting down")
	}

	select {
	case q.messages <- message:
		return nil
	case <-time.After(outboundQueueSendTimeout):
		return fmt.Errorf("timeout sending message")
	}
}

func (q *outboundQueue) Close() {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.messages)
}

func (q *outboundQueue) Messages() <-chan []byte {
	return q.messages
}
