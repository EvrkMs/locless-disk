package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"fogs/proto"
)

// maxResponse bounds one answer: a batch of blocks, or the list of a large disk.
const maxResponse = 512 << 20

// errTaskGone means the server no longer wants this task: it was cancelled.
var errTaskGone = errors.New("task is gone")

// errImageBusy means another task has the image: ask again later.
var errImageBusy = errors.New("image is busy")

// client talks to the FOGs server.
type client struct {
	server  string
	mac     string
	http    *http.Client // short requests
	bulk    *http.Client // image data
	traffic *traffic     // bytes both clients moved
}

func newClient(cfg proto.AgentConfig) *client {
	t := &traffic{}
	counted := countingTransport{base: http.DefaultTransport, t: t}
	return &client{
		server:  cfg.Server,
		mac:     cfg.MAC,
		http:    &http.Client{Timeout: 10 * time.Second, Transport: counted},
		bulk:    &http.Client{Timeout: 5 * time.Minute, Transport: counted},
		traffic: t,
	}
}

// cloneURL builds the address of a clone step.
func (c *client) cloneURL(id int64, step string) string {
	return c.server + "/agent/tasks/" + strconv.FormatInt(id, 10) + "/clone/" + step + "?mac=" + url.QueryEscape(c.mac)
}

// send posts a body and returns the response. errTaskGone means the operator
// cancelled the task.
func (c *client) send(id int64, step, contentType string, body []byte) ([]byte, error) {
	return c.post2(c.cloneURL(id, step), contentType, body)
}

// deployURL builds the address of a deploy step.
func (c *client) deployURL(id int64, step string) string {
	return c.server + "/agent/tasks/" + strconv.FormatInt(id, 10) + "/deploy/" + step + "?mac=" + url.QueryEscape(c.mac)
}

// deployStart asks for the image of a task.
func (c *client) deployStart(id int64) (proto.DeployInfo, error) {
	var info proto.DeployInfo
	data, err := c.post2(c.deployURL(id, "start"), "application/json", nil)
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(data, &info)
}

// deployList fetches the places to write.
func (c *client) deployList(id int64) ([]proto.DeployEntry, error) {
	data, err := c.post2(c.deployURL(id, "list"), "application/json", nil)
	if err != nil {
		return nil, err
	}
	return proto.ParseDeployEntries(data)
}

// deployBlocks fetches stored blocks by their numbers in the list.
func (c *client) deployBlocks(ctx context.Context, id int64, numbers []uint32) ([]byte, error) {
	return c.postContext(ctx, c.deployURL(id, "blocks"), "application/octet-stream", proto.AppendNumbers(nil, numbers))
}

// post2 sends a request for image data and returns the answer.
func (c *client) post2(url, contentType string, body []byte) ([]byte, error) {
	return c.postContext(context.Background(), url, contentType, body)
}

// postContext is post2 for a request that may have to be dropped halfway.
func (c *client) postContext(ctx context.Context, url, contentType string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	res, err := c.bulk.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return nil, err
	}
	switch res.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return data, nil
	case http.StatusGone:
		return nil, errTaskGone
	case http.StatusLocked:
		return nil, errImageBusy
	}
	return nil, fmt.Errorf("сервер ответил %d: %s", res.StatusCode, strings.TrimSpace(string(data)))
}

// cloneClaim asks for the turn to clone the image of a task.
func (c *client) cloneClaim(id int64) error {
	_, err := c.send(id, "claim", "application/json", nil)
	return err
}

// cloneStart opens a clone on the server.
func (c *client) cloneStart(id int64) (proto.CloneInfo, error) {
	var info proto.CloneInfo
	data, err := c.send(id, "start", "application/json", nil)
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(data, &info)
}

// cloneCheck asks which blocks the server lacks.
func (c *client) cloneCheck(id int64, hashes []proto.Sum) ([]byte, error) {
	missing, err := c.send(id, "check", "application/octet-stream", proto.AppendHashes(nil, hashes))
	if err == nil && len(missing) != len(hashes) {
		return nil, fmt.Errorf("сервер ответил на %d блоков из %d", len(missing), len(hashes))
	}
	return missing, err
}

// clonePut sends encoded records.
func (c *client) clonePut(id int64, body []byte) error {
	_, err := c.send(id, "put", "application/octet-stream", body)
	return err
}

// cloneFinish publishes the image.
func (c *client) cloneFinish(id int64, m proto.Manifest) (proto.CloneResult, error) {
	var res proto.CloneResult
	body, err := json.Marshal(m)
	if err != nil {
		return res, err
	}
	data, err := c.send(id, "finish", "application/json", body)
	if err != nil {
		return res, err
	}
	return res, json.Unmarshal(data, &res)
}

// task asks for the active task. It returns nil if there is none.
func (c *client) task() (*proto.AgentTask, error) {
	res, err := c.http.Get(c.server + "/agent/task?mac=" + url.QueryEscape(c.mac))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var t proto.AgentTask
		if err := json.NewDecoder(res.Body).Decode(&t); err != nil {
			return nil, fmt.Errorf("bad task: %w", err)
		}
		return &t, nil
	}
	return nil, fmt.Errorf("server answered %s", res.Status)
}

// progress reports on a running task.
func (c *client) progress(id int64, percent int, stage, message string) error {
	return c.post(id, "progress", proto.AgentProgress{MAC: c.mac, Percent: percent, Stage: stage, Message: message})
}

// finish ends a task.
func (c *client) finish(id int64, ok bool, message string, m *proto.TaskMetrics) error {
	return c.post(id, "finish", proto.AgentResult{MAC: c.mac, OK: ok, Message: message, Metrics: m})
}

func (c *client) post(id int64, action string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	path := c.server + "/agent/tasks/" + strconv.FormatInt(id, 10) + "/" + action
	res, err := c.http.Post(path, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusGone:
		return errTaskGone
	}
	return fmt.Errorf("server answered %s", res.Status)
}

// saveTable keeps the partition table of this PC on the server; nil removes it.
func (c *client) saveTable(table []byte) error {
	_, err := c.post2(c.server+"/agent/table?mac="+url.QueryEscape(c.mac), "application/octet-stream", table)
	return err
}

// failure reports an error of a running task.
func (c *client) failure(id int64, message, details string, m *proto.TaskMetrics) error {
	return c.post(id, "progress", proto.AgentProgress{MAC: c.mac, Stage: proto.StageFailed, Message: message, Details: details, Metrics: m})
}
