package pubsub

import (
	"context"
	"fmt"
	"sync"

	coreapi "github.com/ipfs/interface-go-ipfs-core"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/pkg/errors"
)

type pubSub struct {
	ipfs          coreapi.CoreAPI
	id            peer.ID
	subscriptions map[string]Subscription
	pubSubLock    sync.RWMutex
}

// NewPubSub Creates a new pubsub client
func NewPubSub(is coreapi.CoreAPI, id peer.ID) (Interface, error) {
	if is == nil {
		return nil, errors.New("ipfs is not defined")
	}

	ps := is.PubSub()

	if ps == nil {
		return nil, errors.New("pubsub service is not provided by the current ipfs instance")
	}

	return &pubSub{
		ipfs:          is,
		id:            id,
		subscriptions: map[string]Subscription{},
	}, nil
}

func (p *pubSub) Subscribe(ctx context.Context, topic string) (Subscription, error) {
	p.pubSubLock.RLock()
	sub, ok := p.subscriptions[topic]
	p.pubSubLock.RUnlock()
	if ok {
		return sub, nil
	}

	logger().Debug(fmt.Sprintf("starting pubsub listener for peer %s on topic %s", p.id, topic))

	ctx, cancelFunc := context.WithCancel(ctx)

	s, err := NewSubscription(ctx, p.ipfs, topic, cancelFunc)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create new pubsub subscription")
	}

	p.pubSubLock.Lock()
	p.subscriptions[topic] = s
	p.pubSubLock.Unlock()

	return s, nil
}

func (p *pubSub) Publish(ctx context.Context, topic string, message []byte) error {
	p.pubSubLock.RLock()
	if _, ok := p.subscriptions[topic]; !ok {
		return errors.New("to subscribed to this topic")
	}
	p.pubSubLock.RUnlock()

	return p.ipfs.PubSub().Publish(ctx, topic, message)
}

func (p *pubSub) Close() error {
	p.pubSubLock.RLock()
	subs := p.subscriptions
	p.pubSubLock.RUnlock()

	for _, sub := range subs {
		_ = sub.Close()
	}

	return nil
}

func (p *pubSub) Unsubscribe(topic string) error {
	p.pubSubLock.RLock()
	s, ok := p.subscriptions[topic]
	p.pubSubLock.RUnlock()

	if !ok {
		return errors.New("no subscription found")
	}

	_ = s.Close()

	return nil
}

var _ Interface = &pubSub{}
