package messaging

import (
	"context"
	"fmt"

	"github.com/go-zeromq/zmq4"
)

// ZMQPublisher represents a publisher node that can broadcast low-latency chatter
type ZMQPublisher struct {
	socket zmq4.Socket
}

// NewZMQPublisher creates a PUB socket bound to the specific endpoint (e.g. tcp://*:5555)
func NewZMQPublisher(ctx context.Context, endpoint string) (*ZMQPublisher, error) {
	pub := zmq4.NewPub(ctx)
	if err := pub.Listen(endpoint); err != nil {
		return nil, fmt.Errorf("could not listen on %s: %w", endpoint, err)
	}
	return &ZMQPublisher{socket: pub}, nil
}

func (s *ZMQPublisher) Send(msg string) error {
	return s.socket.Send(zmq4.NewMsgString(msg))
}

func (s *ZMQPublisher) Close() error {
	return s.socket.Close()
}

// ZMQSubscriber subscribes to a ZeroMQ PUB socket
type ZMQSubscriber struct {
	socket zmq4.Socket
}

// NewZMQSubscriber connects a SUB socket and subscribes to all messages ("") 
func NewZMQSubscriber(ctx context.Context, endpoint string) (*ZMQSubscriber, error) {
	sub := zmq4.NewSub(ctx)
	if err := sub.Dial(endpoint); err != nil {
		return nil, fmt.Errorf("could not dial %s: %w", endpoint, err)
	}
	
	err := sub.SetOption(zmq4.OptionSubscribe, "")
	if err != nil {
		return nil, err
	}
	return &ZMQSubscriber{socket: sub}, nil
}

func (c *ZMQSubscriber) Receive() (string, error) {
	msg, err := c.socket.Recv()
	if err != nil {
		return "", err
	}
	return string(msg.Bytes()), nil
}

func (c *ZMQSubscriber) Close() error {
	return c.socket.Close()
}
