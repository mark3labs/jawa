package reportprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/nats-io/nats.go"
)

// Client waits for a matching post-commit ACK. Timeouts retry the SAME bytes;
// they never imply rejection or success. The receiver deduplicates durably.
type Client struct {
	Conn *nats.Conn
	Root string
}

func (c Client) Submit(ctx context.Context, r Request) (Ack, error) {
	if err := r.Validate(); err != nil {
		return Ack{}, err
	}
	if c.Conn == nil {
		return Ack{}, errors.New("typed report transport unavailable")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return Ack{}, err
	}
	for attempt := range 3 {
		wait, cancel := context.WithTimeout(ctx, 5*time.Second)
		msg, requestErr := c.Conn.RequestWithContext(wait, Subject(c.Root), data)
		cancel()
		if requestErr == nil {
			var ack Ack
			if json.Unmarshal(msg.Data, &ack) != nil || ack.Version != Version || ack.ReportID != r.ReportID {
				return Ack{}, errors.New("invalid typed report commit acknowledgement")
			}
			if !ack.Accepted || ack.Error != "" {
				return ack, errors.New("typed report rejected: " + ack.Error)
			}
			return ack, nil
		}
		err = requestErr
		if ctx.Err() != nil {
			return Ack{}, ctx.Err()
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Ack{}, ctx.Err()
		case <-timer.C:
		}
	}
	return Ack{}, errors.New("typed report commit unconfirmed; retry the identical report: " + err.Error())
}
