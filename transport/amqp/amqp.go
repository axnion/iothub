package amqp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/amenzhinsky/iothub/eventhub"
	"github.com/amenzhinsky/iothub/transport"
	"github.com/satori/go.uuid"
	"pack.ag/amqp"
)

// TransportOption is transport configuration option.
type TransportOption func(tr *Transport) error

// WithLogger overrides transport logger.
func WithLogger(l *log.Logger) TransportOption {
	return func(c *Transport) error {
		c.logger = l
		return nil
	}
}

// New creates new amqp iothub transport.
func New(opts ...TransportOption) (transport.Transport, error) {
	tr := &Transport{
		c2ds:   make(chan *transport.Event, 10),
		done:   make(chan struct{}),
		logger: log.New(os.Stdout, "[amqp] ", 0),
	}
	for _, opt := range opts {
		if err := opt(tr); err != nil {
			return nil, err
		}
	}
	return tr, nil
}

type Transport struct {
	mu     sync.RWMutex
	conn   *eventhub.Client
	logger *log.Logger

	c2ds chan *transport.Event
	done chan struct{}
}

func (tr *Transport) Connect(
	ctx context.Context,
	tlsConfig *tls.Config,
	deviceID string,
	sasFunc transport.AuthFunc,
) error {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.conn != nil {
		return errors.New("already connected")
	}

	// SAS uri for amqp has to be: hostname + "/devices/" + deviceID
	hostname, token, err := sasFunc(ctx, "/devices/"+deviceID)
	if err != nil {
		return err
	}
	c, err := eventhub.Dial(hostname, tlsConfig)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			c.Close()
		}
	}()

	if err := c.PutTokenContinuously(
		ctx,
		hostname+"/devices/"+deviceID,
		token,
		tr.done,
	); err != nil {
		return err
	}

	//TODO: ctx, cancel := context.WithCancel(context.Background())

	c2d, err := c.Sess().NewReceiver(
		amqp.LinkSourceAddress("/devices/" + deviceID + "/messages/devicebound"),
	)
	if err != nil {
		return err
	}
	go func() {
		for {
			msg, err := c2d.Receive(context.Background())
			if err != nil {
				select {
				case <-tr.done:
					return
				default:
				}

				// TODO: disconnect errors can occur here
				panic(err)
			}

			props := make(map[string]string, len(msg.ApplicationProperties))
			for k, v := range msg.ApplicationProperties {
				props[k] = fmt.Sprint(v)
			}

			select {
			case tr.c2ds <- &transport.Event{
				DeviceID:   deviceID,
				Payload:    msg.Data[0],
				Properties: props,
			}:
			case <-tr.done:
			}
		}
	}()

	tr.conn = c
	return nil
}

func (tr *Transport) IsNetworkError(err error) bool {
	return false
}

func (tr *Transport) PublishEvent(ctx context.Context, event *transport.Event) error {
	if err := tr.checkConnection(); err != nil {
		return err
	}

	target := "/devices/" + event.DeviceID + "/messages/events"
	send, err := tr.conn.Sess().NewSender(
		amqp.LinkTargetAddress(target),
	)
	if err != nil {
		return err
	}
	defer send.Close()

	ap := make(map[string]interface{}, len(event.Properties))
	for k, v := range event.Properties {
		ap[k] = v
	}
	return send.Send(ctx, &amqp.Message{
		Data: [][]byte{event.Payload},
		Properties: &amqp.MessageProperties{
			To:            target,
			MessageID:     uuid.Must(uuid.NewV4()).String(),
			CorrelationID: uuid.Must(uuid.NewV4()).String(),
		},
		ApplicationProperties: ap,
	})
}

func (tr *Transport) C2D() chan *transport.Event {
	return tr.c2ds
}

func (tr *Transport) DMI() chan *transport.Call {
	return nil
}

func (tr *Transport) DSC() chan []byte {
	return nil
}

func (tr *Transport) RespondDirectMethod(ctx context.Context, rid string, code int, payload []byte) error {
	return nil
}

func (tr *Transport) RetrieveTwinProperties(ctx context.Context) (payload []byte, err error) {
	return nil, nil
}

func (tr *Transport) UpdateTwinProperties(ctx context.Context, payload []byte) (version int, err error) {
	return 0, nil
}

func (tr *Transport) checkConnection() error {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	if tr.conn == nil {
		return errors.New("not connected")
	}
	return nil
}

func (tr *Transport) Close() error {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	select {
	case <-tr.done:
		return nil
	default:
		close(tr.done)
	}
	return tr.conn.Close()
}
