package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

const (
	taskOrderUsage         = "usage: tasks order <task-id> --words <the operator's words> --idempotency-key <key>"
	taskOrderAckUsage      = "usage: tasks order-ack <order-id> --comment <text>"
	taskOrderWithdrawUsage = "usage: tasks order-withdraw <order-id> --reason <text>"
)

// taskOrderCmd serves "tasks order <task-id> --words ... --idempotency-key ..."
// (hk 5125): the operator's "set going" for a task that has not started.
func taskOrderCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks order", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	words := fs.String("words", "", "the operator's words, verbatim")
	key := fs.String("idempotency-key", "", "idempotency key; the same key and words replay the first order")
	ordered, err := splitArgs(args[1:], "words", "idempotency-key")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 1 || *words == "" || *key == "" {
		return errors.New(taskOrderUsage)
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id < 1 {
		return errors.New("task id must be positive")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := c.CreateTaskOrder(ctx, id, *words, *key)
	if err != nil {
		return err
	}
	return printJSON(out, res)
}

// taskOrderAckCmd serves "tasks order-ack <order-id> --comment <text>": the
// target lane's acknowledgement, which clears the order's lane wait.
func taskOrderAckCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks order-ack", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	comment := fs.String("comment", "", "comment recorded on the task")
	ordered, err := splitArgs(args[1:], "comment")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 1 || *comment == "" {
		return errors.New(taskOrderAckUsage)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := c.AckTaskOrder(ctx, fs.Arg(0), *comment)
	if err != nil {
		return err
	}
	return printJSON(out, res)
}

// taskOrderWithdrawCmd serves "tasks order-withdraw <order-id> --reason <text>":
// the operator withdraws an order that is still open.
func taskOrderWithdrawCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks order-withdraw", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	reason := fs.String("reason", "", "why the order is withdrawn")
	ordered, err := splitArgs(args[1:], "reason")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 1 || *reason == "" {
		return errors.New(taskOrderWithdrawUsage)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := c.WithdrawTaskOrder(ctx, fs.Arg(0), *reason)
	if err != nil {
		return err
	}
	return printJSON(out, res)
}

// taskShowOutput is "tasks show": the task, and the orders still waiting on
// their lane.
type taskShowOutput struct {
	store.Task
	OpenOrders []store.TaskOrder `json:"open_orders"`
}

// openTaskOrders lists a task's open orders for tasks show. A server that
// predates the orders route answers 404, which reads as no orders.
func openTaskOrders(ctx context.Context, c *remote.Client, id int64) ([]store.TaskOrder, error) {
	orders, err := c.ListTaskOrders(ctx, id, true)
	var he *remote.HTTPError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		return []store.TaskOrder{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list open orders: %w", err)
	}
	return orders, nil
}
