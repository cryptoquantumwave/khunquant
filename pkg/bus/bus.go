package bus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// ErrBusClosed is returned when publishing to a closed MessageBus.
var ErrBusClosed = errors.New("message bus closed")

const defaultBusBufferSize = 64

type MessageBus struct {
	inbound       chan InboundMessage
	outbound      chan OutboundMessage
	outboundMedia chan OutboundMediaMessage

	closeOnce sync.Once
	done      chan struct{}
	closed    atomic.Bool
	// pubMu is read-locked by every in-flight publish and write-locked by Close
	// while it closes the channels, so a send can never reach a closed channel.
	// (A WaitGroup cannot do this job: Add racing Wait on a zero counter is a
	// documented misuse, and let a publisher slip in after Wait returned.)
	pubMu sync.RWMutex
}

func NewMessageBus() *MessageBus {
	return &MessageBus{
		inbound:       make(chan InboundMessage, defaultBusBufferSize),
		outbound:      make(chan OutboundMessage, defaultBusBufferSize),
		outboundMedia: make(chan OutboundMediaMessage, defaultBusBufferSize),
		done:          make(chan struct{}),
	}
}

func publish[T any](ctx context.Context, mb *MessageBus, ch chan T, msg T) error {
	mb.pubMu.RLock()
	defer mb.pubMu.RUnlock()

	// Checked under the read lock: Close cannot be closing the channels now.
	if mb.closed.Load() {
		return ErrBusClosed
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-mb.done:
		return ErrBusClosed
	default:
	}

	select {
	case ch <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-mb.done:
		return ErrBusClosed
	}
}

func (mb *MessageBus) PublishInbound(ctx context.Context, msg InboundMessage) error {
	return publish(ctx, mb, mb.inbound, msg)
}

func (mb *MessageBus) InboundChan() <-chan InboundMessage {
	return mb.inbound
}

func (mb *MessageBus) PublishOutbound(ctx context.Context, msg OutboundMessage) error {
	return publish(ctx, mb, mb.outbound, msg)
}

func (mb *MessageBus) OutboundChan() <-chan OutboundMessage {
	return mb.outbound
}

func (mb *MessageBus) PublishOutboundMedia(ctx context.Context, msg OutboundMediaMessage) error {
	return publish(ctx, mb, mb.outboundMedia, msg)
}

func (mb *MessageBus) OutboundMediaChan() <-chan OutboundMediaMessage {
	return mb.outboundMedia
}

func (mb *MessageBus) Close() {
	mb.closeOnce.Do(func() {
		// notify all blocked publishers to exit
		close(mb.done)
		mb.closed.Store(true)

		// wait for all ongoing Publish calls to finish, ensuring all messages have been sent to channels or exited;
		// publishers arriving afterwards see closed under the read lock and return ErrBusClosed
		mb.pubMu.Lock()
		defer mb.pubMu.Unlock()

		// close channels safely
		close(mb.inbound)
		close(mb.outbound)
		close(mb.outboundMedia)

		// clean up any remaining messages in channels
		drained := 0
		for range mb.inbound {
			drained++
		}
		for range mb.outbound {
			drained++
		}
		for range mb.outboundMedia {
			drained++
		}

		if drained > 0 {
			logger.DebugCF("bus", "Drained buffered messages during close", map[string]any{
				"count": drained,
			})
		}
	})
}
