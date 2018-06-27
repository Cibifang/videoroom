//
//
// message.go

package main

import (
	"log"
	"strings"

	"github.com/tidwall/gjson"
	"janus"
)

type message struct {
	desc    string
	content interface{}
}

type handler interface {
	MsgChan() chan message
}

func sendMessage(msg message, msgChan chan message) {
	msgChan <- msg
}

func newJanusSession(j *janus.Janus) uint64 {
	tid := j.NewTransaction()

	msg := make(map[string]interface{})
	msg["janus"] = "create"
	msg["transaction"] = tid

	j.Send(msg)
	reqChan, ok := j.MsgChan(tid)
	if !ok {
		log.Printf("newJanusSession: can't find channel for tid %s", tid)
	}

	req := <-reqChan
	if gjson.GetBytes(req, "janus").String() != "success" {
		log.Printf("newJanusSession: failed, fail message: `%s`", req)
		return 0
	}

	sessId := gjson.GetBytes(req, "data.id").Uint()
	j.NewSess(sessId)

	log.Printf("newJanusSession: new janus session `%d` success", sessId)
	return sessId
}

func attachVideoroom(j *janus.Janus, sid uint64) uint64 {
	janusSess, _ := j.Session(sid)
	tid := janusSess.NewTransaction()

	msg := make(map[string]interface{})
	msg["janus"] = "attach"
	msg["plugin"] = "janus.plugin.videoroom"
	msg["transaction"] = tid
	msg["session_id"] = sid

	j.Send(msg)
	reqChan, ok := janusSess.MsgChan(tid)
	if !ok {
		log.Printf("attachVideoroom: can't find channel for tid %s", tid)
	}

	req := <-reqChan
	if gjson.GetBytes(req, "janus").String() != "success" {
		log.Printf("attachVideoroom: failed, fail message: `%s`", req)
		return 0
	}

	handleId := gjson.GetBytes(req, "data.id").Uint()
	janusSess.Attach(handleId)

	log.Printf("attachVideoroom: session `%d` attach handler `%d` success",
		sid, handleId)
	return handleId
}

func createRoom(j *janus.Janus, sid uint64, hid uint64) uint64 {
	janusSess, _ := j.Session(sid)
	tid := janusSess.NewTransaction()

	msg := make(map[string]interface{})
	body := make(map[string]interface{})
	body["request"] = "create"
	body["audiocodec"] = "opus"
	body["videocodec"] = "h264"
	// Don't need limit now, so just set to 20
	body["publishers"] = 20
	msg["janus"] = "message"
	msg["session_id"] = sid
	msg["handle_id"] = hid
	msg["transaction"] = tid
	msg["body"] = body

	j.Send(msg)
	reqChan, ok := janusSess.MsgChan(tid)
	if !ok {
		log.Printf("createRoom: can't find channel for tid %s", tid)
		return 0
	}

	req := <-reqChan

	room := gjson.GetBytes(req, "plugindata.data.room").Uint()
	log.Printf("createRoom: add room `%d` for id `%d`", room, sid)
	return room
}

func publish(j *janus.Janus, sid, hid, room uint64, display string) []byte {
	janusSess, _ := j.Session(sid)
	tid := janusSess.NewTransaction()

	msg := make(map[string]interface{})
	body := make(map[string]interface{})
	body["request"] = "join"
	body["room"] = room
	body["ptype"] = "publisher"
	body["display"] = display
	msg["janus"] = "message"
	msg["transaction"] = tid
	msg["session_id"] = sid
	msg["handle_id"] = hid
	msg["body"] = body

	j.Send(msg)
	reqChan, ok := janusSess.MsgChan(tid)
	if !ok {
		log.Printf("publish: can't find channel for tid %s", tid)
	}

	for {
		req := <-reqChan
		switch gjson.GetBytes(req, "janus").String() {
		case "ack":
			log.Printf("publish: receive ack")
		case "error":
			log.Printf("publish: receive error msg `%s`", req)
			return nil
		case "event":
			log.Printf("publish: receive success msg `%s`", req)
			return req
		}
	}
}

func listen(j *janus.Janus, sid, hid, room, feed, privateId uint64) string {
	janusSess, _ := j.Session(sid)
	tid := janusSess.NewTransaction()

	msg := make(map[string]interface{})
	body := make(map[string]interface{})
	body["request"] = "join"
	body["room"] = room
	body["ptype"] = "listener"
	body["feed"] = feed
	body["private_id"] = privateId
	msg["janus"] = "message"
	msg["transaction"] = tid
	msg["session_id"] = sid
	msg["handle_id"] = hid
	msg["body"] = body

	j.Send(msg)
	reqChan, ok := janusSess.MsgChan(tid)
	if !ok {
		log.Printf("listen: can't find channel for tid %s", tid)
	}

	for {
		req := <-reqChan
		switch gjson.GetBytes(req, "janus").String() {
		case "ack":
			log.Printf("listen: receive ack")
		case "error":
			log.Printf("listen: receive err msg `%s`", req)
			return ""
		case "event":
			log.Printf("listen: receive success msg `%s`", req)
			return gjson.GetBytes(req, "jsep.sdp").String()
		}
	}
}

func configure(j *janus.Janus, sid uint64, hid uint64, sdp string) string {
	janusSess, _ := j.Session(sid)
	tid := janusSess.NewTransaction()

	msg := make(map[string]interface{})
	body := make(map[string]interface{})
	jsep := make(map[string]interface{})
	body["request"] = "configure"
	body["audio"] = true
	body["video"] = true
	jsep["type"] = "offer"
	jsep["sdp"] = sdp
	msg["janus"] = "message"
	msg["transaction"] = tid
	msg["session_id"] = sid
	msg["handle_id"] = hid
	msg["body"] = body
	msg["jsep"] = jsep

	j.Send(msg)
	reqChan, ok := janusSess.MsgChan(tid)
	if !ok {
		log.Printf("configure: can't find channel for tid %s", tid)
	}

	for {
		req := <-reqChan
		switch gjson.GetBytes(req, "janus").String() {
		case "ack":
			log.Printf("configure: receive ack")
		case "error":
			log.Printf("configure: receive err msg `%s`", req)
			return ""
		case "event":
			log.Printf("configure: receive success msg `%s`", req)
			return gjson.GetBytes(req, "jsep.sdp").String()
		}
	}
}

func start(j *janus.Janus, sid uint64, hid uint64, room uint64, sdp string) {
	janusSess, _ := j.Session(sid)
	tid := janusSess.NewTransaction()

	msg := make(map[string]interface{})
	body := make(map[string]interface{})
	jsep := make(map[string]interface{})
	body["request"] = "start"
	body["room"] = room
	jsep["type"] = "answer"
	jsep["sdp"] = sdp
	msg["janus"] = "message"
	msg["transaction"] = tid
	msg["session_id"] = sid
	msg["handle_id"] = hid
	msg["body"] = body
	msg["jsep"] = jsep

	j.Send(msg)
	reqChan, ok := janusSess.MsgChan(tid)
	if !ok {
		log.Printf("start: can't find channel for tid %s", tid)
	}

	for {
		req := <-reqChan
		switch gjson.GetBytes(req, "janus").String() {
		case "ack":
			log.Printf("start: receive ack")
		case "error":
			log.Printf("start: receive err msg `%s`", req)
			return
		case "event":
			log.Printf("start: receive success msg `%s`", req)
			return
		}
	}
}

func candidate(j *janus.Janus, sid, hid, index uint64, date, mid string) {
	janusSess, _ := j.Session(sid)
	tid := janusSess.NewTransaction()

	msg := make(map[string]interface{})
	candidate := make(map[string]interface{})
	candidate["candidate"] = date
	candidate["sdpMid"] = mid
	candidate["sdpMLineIndex"] = index
	msg["janus"] = "trickle"
	msg["session_id"] = sid
	msg["handle_id"] = hid
	msg["transaction"] = tid
	msg["candidate"] = candidate

	j.Send(msg)
	reqChan, ok := janusSess.MsgChan(tid)
	if !ok {
		log.Printf("candidate: can't find channel for tid %s", tid)
	}

	req := <-reqChan
	log.Printf("candidate: receive from channel: %s", req)
}

func completeCandidate(j *janus.Janus, sid uint64, hid uint64) {
	janusSess, _ := j.Session(sid)
	tid := janusSess.NewTransaction()

	msg := make(map[string]interface{})
	candidate := make(map[string]interface{})
	candidate["completed"] = true
	msg["janus"] = "trickle"
	msg["session_id"] = sid
	msg["handle_id"] = hid
	msg["transaction"] = tid
	msg["candidate"] = candidate

	j.Send(msg)
	reqChan, ok := janusSess.MsgChan(tid)
	if !ok {
		log.Printf("completeCandidate: can't find channel for tid %s", tid)
	}

	req := <-reqChan
	log.Printf("completeCandidate: receive from channel: %s", req)
}

func unpublish(j *janus.Janus, sid uint64, hid uint64) {
	janusSess, _ := j.Session(sid)
	tid := janusSess.NewTransaction()

	msg := make(map[string]interface{})
	body := make(map[string]interface{})
	body["request"] = "unpublish"
	msg["janus"] = "message"
	msg["transaction"] = tid
	msg["session_id"] = sid
	msg["handle_id"] = hid
	msg["body"] = body

	j.Send(msg)
	reqChan, ok := janusSess.MsgChan(tid)
	if !ok {
		log.Printf("unpublish: can't find channel for tid %s", tid)
	}

	for {
		req := <-reqChan
		switch gjson.GetBytes(req, "janus").String() {
		case "ack":
			log.Printf("unpublish: receive ack")
		case "error":
			log.Printf("unpublish: recevie error msg `%s`", req)
			return
		case "event":
			log.Printf("unpublish: receive success msg `%s`", req)
			return
		}
	}
}

func detach(j *janus.Janus, sid uint64, hid uint64) {
	janusSess, _ := j.Session(sid)
	tid := janusSess.NewTransaction()

	msg := make(map[string]interface{})
	msg["janus"] = "detach"
	msg["transaction"] = tid
	msg["session_id"] = sid
	msg["handle_id"] = hid

	j.Send(msg)
	reqChan, ok := janusSess.MsgChan(tid)
	if !ok {
		log.Printf("detach: can't find channel for tid %s", tid)
		return
	}

	req := <-reqChan
	if gjson.GetBytes(req, "janus").String() != "success" {
		log.Printf("detach: failed, fail message: `%s`", req)
		return
	}
	log.Printf("detach: detach handle `%d` from `%d` success", hid, sid)
}

func keepalive(j *janus.Janus, sid uint64) {
	janusSess, _ := j.Session(sid)
	tid := janusSess.NewTransaction()

	msg := make(map[string]interface{})
	msg["janus"] = "keepalive"
	msg["session_id"] = sid
	msg["transaction"] = tid

	j.Send(msg)
	reqChan, ok := janusSess.MsgChan(tid)
	if !ok {
		log.Printf("keepalive: can't find channel for tid %s", tid)
	}

	req := <-reqChan
	log.Printf("keepalive: recv ack response: `%s`", req)
}

func destroy(j *janus.Janus, sid uint64) {
	janusSess, _ := j.Session(sid)
	tid := janusSess.NewTransaction()

	msg := make(map[string]interface{})
	msg["janus"] = "destroy"
	msg["session_id"] = sid
	msg["transaction"] = tid

	j.Send(msg)
	reqChan, ok := janusSess.MsgChan(tid)
	if !ok {
		log.Printf("destroy: can't find channel for tid %s", tid)
	}

	req := <-reqChan
	if gjson.GetBytes(req, "janus").String() != "success" {
		log.Printf("destroy: failed, fail message: `%s`", req)
		return
	}
	log.Printf("destroy: destroy session `%d` success", sid)
}

func getCandidatesFromSdp(sdp string) []string {
	var candidate []string
	for _, sdpItem := range strings.Split(sdp, "\r\n") {
		pair := strings.SplitN(sdpItem, "=", 2)
		if len(pair) != 2 {
			continue
		}

		valuePair := strings.SplitN(pair[1], ":", 2)
		if len(valuePair) != 2 {
			continue
		}

		if valuePair[0] != "candidate" {
			continue
		}

		log.Printf(
			"getFirstCandidateFromSdp: find candidate `%s` from line `%s`",
			valuePair[1], sdpItem)
		candidate = append(candidate, valuePair[1])
	}

	return candidate
}
