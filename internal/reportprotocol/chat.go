package reportprotocol

import (
	"context"
	"encoding/json"
	"errors"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/nats-io/nats.go"
)

// Chat is a pinned, explicit input command. Token authenticates transport and
// is excluded from durable identity. Reconcile never starts an unseen command.
type Chat struct {
	Version   int           `json:"version"`
	ID        string        `json:"id"`
	Target    client.Target `json:"target"`
	Text      string        `json:"text"`
	Followup  bool          `json:"followup"`
	Seq       int           `json:"seq"`
	TurnID    string        `json:"turn_id"`
	Token     string        `json:"token,omitempty"`
	Reconcile bool          `json:"reconcile,omitempty"`
}

type ChatReceipt struct {
	UserIndex *int            `json:"user_index,omitempty"`
	Version   int             `json:"version"`
	ID        string          `json:"id"`
	Target    client.Target   `json:"target"`
	Status    string          `json:"status"`
	Failure   string          `json:"failure,omitempty"`
	TurnID    string          `json:"turn_id,omitempty"`
	Seq       int             `json:"seq"`
	Outcome   *client.Outcome `json:"outcome,omitempty"`
}

func ChatSubject(root, agent string) string { return root + ".run_chat.v1." + agent }

func SendChat(ctx context.Context, nc *nats.Conn, req Chat) (ChatReceipt, error) {
	var receipt ChatReceipt
	data, err := json.Marshal(req)
	if err != nil {
		return receipt, err
	}
	msg, err := nc.RequestWithContext(ctx, ChatSubject("bonnie", req.Target.AgentID), data)
	if err != nil {
		return receipt, err
	}
	if len(msg.Data) > 1<<20 {
		return receipt, errors.New("chat receipt too large")
	}
	if err = json.Unmarshal(msg.Data, &receipt); err != nil {
		return receipt, err
	}
	if receipt.Version != 1 || receipt.ID != req.ID || receipt.Target != req.Target {
		return receipt, errors.New("invalid chat receipt")
	}
	return receipt, nil
}
