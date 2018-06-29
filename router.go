//
//
// router.go

package main

import (
	"encoding/json"
	"log"
	"strconv"
	"time"

	simplejson "github.com/bitly/go-simplejson"
	uuid "github.com/satori/go.uuid"
	"github.com/tidwall/gjson"
	"janus"
	"rtclib"
)

type router struct {
	remote  string
	local   string
	msgChan chan message
	close   chan bool
	global  *globalCtx
	process map[string](chan message)
	feeds   map[feed]string

	sid       uint64
	janusConn *janus.Janus
}

type routerPub struct {
	rou        *router
	hid        uint64
	dialogueID string
}

type routerLis struct {
	rou        *router
	hid        uint64
	feed       feed
	dialogueID string
	rtcRoom    string
}

func newRouter(remote string, conn *janus.Janus, ctx *globalCtx) *router {
	sid := newJanusSession(conn)
	router := &router{
		sid:       sid,
		local:     rtclib.Realm(),
		remote:    remote,
		janusConn: conn,
		msgChan:   make(chan message),
		close:     make(chan bool),
		global:    ctx,
		process:   make(map[string](chan message)),
		feeds:     make(map[feed]string),
	}

	go router.keepalive()
	go router.main()
	return router
}

func (r *router) MsgChan() chan message {
	return r.msgChan
}

func (r *router) new(janusRoom uint64) {
	hid := attachVideoroom(r.janusConn, r.sid)
	publishMsg := publish(r.janusConn, r.sid, hid, janusRoom, "router")
	if publishMsg == nil {
		log.Printf("router: room `%d` has no publisher now", janusRoom)
		return
	}

	data := gjson.GetBytes(publishMsg, "plugindata.data")
	publishers := data.Get("publishers").Array()
	for _, pubMsg := range publishers {
		f := feed{
			room:    janusRoom,
			pid:     pubMsg.Get("id").Uint(),
			display: pubMsg.Get("display").String(),
		}
		r.notifyFeed(f)
	}

	return
}

func (r *router) main() {
	janusSess, _ := r.janusConn.Session(r.sid)
	janusMsg := janusSess.DefaultMsgChan()

	for {
		select {
		case <-r.close:
			return
		case msg := <-r.msgChan:
			log.Printf("router: receive msg `%+v`", msg)
			r.messageHandle(msg)
			log.Printf("router: msg `%+v` process end", msg)
		case msg := <-janusMsg:
			r.janusMessage(msg)
		}
	}
}

func (r *router) janusMessage(msg []byte) {
	switch gjson.GetBytes(msg, "janus").String() {
	case "event":
		plugindata := gjson.GetBytes(msg, "plugindata")
		if !plugindata.Exists() {
			return
		}

		data := plugindata.Get("data")
		if !data.Exists() {
			return
		}

		// We just handle event message now.
		if data.Get("videoroom").String() != "event" {
			return
		}

		if data.Get("publishers").Exists() {
			publishers := data.Get("publishers").Array()
			for _, pubMsg := range publishers {
				feed := feed{
					room:    data.Get("room").Uint(),
					pid:     pubMsg.Get("id").Uint(),
					display: pubMsg.Get("display").String(),
				}
				r.notifyFeed(feed)
			}
		} else if data.Get("unpublished").Exists() {
			feed := feed{
				room: data.Get("room").Uint(),
				pid:  data.Get("unpublished").Uint(),
			}
			r.notifyUnpublish(feed)
		}
	}
}

func (r *router) messageHandle(msg message) {
	switch msg.desc {
	case "jsip":
		jsip := msg.content.(*rtclib.JSIP)
		id := jsip.DialogueID
		msgChan, exist := r.process[id]
		if exist {
			go sendMessage(msg, msgChan)
			return
		}

		if jsip.Type != rtclib.INVITE || jsip.Code != 0 {
			log.Printf("router: no process for id `%s`", id)
			return
		}

		msgChan = make(chan message)
		r.process[id] = msgChan

		go r.publish(msgChan, id)
		go sendMessage(msg, msgChan)
	case "notifyFeed":
		feed := msg.content.(feed)
		_, exist := r.feeds[feed]
		if exist {
			return
		}

		id := r.newDialogueID()
		for {
			_, exist := r.process[id]
			if exist {
				log.Printf("router: id `%s` already exist", id)
				id = r.newDialogueID()
			}
			break
		}
		msgChan := make(chan message)
		r.process[id] = msgChan
		r.feeds[feed] = id
		r.global.handlers.Store(id, r)
		go r.listen(msgChan, id, feed)
	case "notifyUnpublish":
		unpub := msg.content.(feed)

		// Unpublish message don't have display value.
		id := ""
		for feed := range r.feeds {
			if feed.pid == unpub.pid && feed.room == unpub.room {
				id = r.feeds[feed]
				delete(r.feeds, feed)
				break
			}
		}

		if id != "" {
			msgChan := r.process[id]
			go sendMessage(msg, msgChan)
		}

		return
	case "registerPublisher":
		pubMsg := msg.content.(map[string]interface{})
		id := pubMsg["id"].(string)
		feed := pubMsg["feed"].(feed)
		r.feeds[feed] = id
	case "notifyQuit":
		id := msg.content.(string)
		delete(r.process, id)
		r.global.handlers.Delete(id)
		// TODO: Divide Pub and Listen to finish.
		if len(r.process) == 0 {
			log.Printf("messageHandle: all process is done")
			r.finish()
		}

		for key, value := range r.feeds {
			if value == id {
				delete(r.feeds, key)
				break
			}
		}
	}
}

func (r *router) publish(msgChan chan message, id string) {
	defer r.notifyQuit(id)

	hid := attachVideoroom(r.janusConn, r.sid)
	pub := routerPub{
		rou:        r,
		hid:        hid,
		dialogueID: id,
	}

	for {
		select {
		case <-r.close:
			return
		case msg := <-msgChan:
			if !pub.messageHandle(msg) {
				log.Printf("publish(r): process `%d` quit", hid)
				return
			}
		}
	}
}

func (pub *routerPub) messageHandle(msg message) bool {
	r := pub.rou
	switch msg.desc {
	case "jsip":
		jsip := msg.content.(*rtclib.JSIP)
		switch jsip.Type {
		case rtclib.INVITE:
			if jsip.Body == nil {
				log.Printf("publish: unexpected INVITE msg `%+v`", jsip)
				return false
			}
			body := jsip.Body.(*simplejson.Json)
			rtcRoom, err := body.Get("room").String()
			if err != nil {
				log.Printf("publish(r): not room in message `%+v`", jsip)
				return false
			}
			display := jsip.From
			offer, err := body.Get("sdp").String()
			if err != nil {
				log.Printf("publish(r): not sdp in message `%+v`", jsip)
				return false
			}

			room, exist := r.getRoom(rtcRoom)
			if !exist {
				log.Printf("publish(r): no room record for `%s`", rtcRoom)
				return false
			}
			publishMsg := publish(r.janusConn, r.sid, pub.hid, room, display)
			if publishMsg == nil {
				return false
			}

			data := gjson.GetBytes(publishMsg, "plugindata.data")
			pid := data.Get("id").Uint()
			f := feed{room: room, pid: pid, display: display}
			r.registerPublisher(pub.dialogueID, f)
			publishers := data.Get("publishers").Array()
			for _, pubMsg := range publishers {
				f := feed{
					room:    room,
					pid:     pubMsg.Get("id").Uint(),
					display: pubMsg.Get("display").String(),
				}
				r.notifyFeed(f)
			}

			answer := configure(r.janusConn, r.sid, pub.hid, offer)
			if answer == "" {
				return false
			}

			candidates := getCandidatesFromSdp(answer)
			index := 0
			for _, candidate := range candidates {
				body := make(map[string]interface{})
				subBody := make(map[string]interface{})
				subBody["candidate"] = candidate
				subBody["sdpMLineIndex"] = index
				subBody["sdmMid"] = "sdp"
				body["candidate"] = subBody
				requestRawMsg := make(map[string]interface{})
				requestRawMsg["P-Asserted-Identity"] = jsip.RequestURI
				requestRawMsg["RouterMessage"] = true

				request := &rtclib.JSIP{
					Type:       rtclib.INFO,
					RequestURI: r.remote,
					From:       jsip.To,
					To:         rtcRoom,
					DialogueID: jsip.DialogueID,
					Body:       body,
					RawMsg:     requestRawMsg,
				}
				rtclib.SendMsg(request)
			}
			if len(candidates) > 0 {
				body := make(map[string]interface{})
				subBody := make(map[string]interface{})
				subBody["completed"] = true
				body["candidate"] = subBody
				requestRawMsg := make(map[string]interface{})
				requestRawMsg["P-Asserted-Identity"] = jsip.RequestURI
				requestRawMsg["RouterMessage"] = true

				request := &rtclib.JSIP{
					Type:       rtclib.INFO,
					RequestURI: r.remote,
					From:       jsip.To,
					To:         rtcRoom,
					DialogueID: jsip.DialogueID,
					Body:       body,
					RawMsg:     requestRawMsg,
				}
				rtclib.SendMsg(request)
			}

			responseBody := make(map[string]interface{})
			responseBody["type"] = "answer"
			responseBody["sdp"] = answer
			responseRawMsg := make(map[string]interface{})
			responseRawMsg["P-Asserted-Identity"] = jsip.RequestURI
			responseRawMsg["RouterMessage"] = true

			response := &rtclib.JSIP{
				Type:       rtclib.INVITE,
				Code:       200,
				From:       jsip.To,
				To:         rtcRoom,
				CSeq:       jsip.CSeq,
				DialogueID: jsip.DialogueID,
				RawMsg:     responseRawMsg,
				Body:       responseBody,
			}

			rtclib.SendMsg(response)
			return true
		case rtclib.INFO:
			if jsip.Code != 0 {
				return true
			}
			if jsip.Body == nil {
				log.Printf("publish: unexpected INFO msg `%+v`", jsip)
				return false
			}
			body := jsip.Body.(*simplejson.Json)
			date, exist := body.CheckGet("candidate")
			if exist == false {
				/* Now the info only transport candidate */
				log.Printf("publish: not found candidate from INFO msg")
				return false
			}

			completed, err := date.Get("completed").Bool()
			if err == nil && completed == true {
				completeCandidate(r.janusConn, r.sid, pub.hid)
			} else {
				value, _ := date.Get("candidate").String()
				mid, _ := date.Get("sdpMid").String()
				index, _ := date.Get("sdpMLineIndex").Uint64()
				candidate(r.janusConn, r.sid, pub.hid, index, value, mid)
			}

			responseRawMsg := make(map[string]interface{})
			responseRawMsg["P-Asserted-Identity"] = jsip.RequestURI
			responseRawMsg["RouterMessage"] = true
			response := &rtclib.JSIP{
				Type:       rtclib.INFO,
				Code:       200,
				From:       jsip.To,
				To:         jsip.From,
				CSeq:       jsip.CSeq,
				DialogueID: jsip.DialogueID,
				RawMsg:     responseRawMsg,
			}
			rtclib.SendMsg(response)
			return true
		case rtclib.BYE:
			unpublish(r.janusConn, r.sid, pub.hid)
			detach(r.janusConn, r.sid, pub.hid)

			return false
		}
	}
	return true
}

func (r *router) listen(msgChan chan message, id string, feed feed) {
	defer r.notifyQuit(id)

	pubHid := attachVideoroom(r.janusConn, r.sid)
	// Need private_id, so must join as publisher first.
	joinMsg := publish(r.janusConn, r.sid, pubHid, feed.room, feed.display)
	data := gjson.GetBytes(joinMsg, "plugindata.data")
	privateID := data.Get("private_id").Uint()

	hid := attachVideoroom(r.janusConn, r.sid)
	offer := listen(r.janusConn, r.sid, hid, feed.room, feed.pid, privateID)

	rtcRoom, exist := r.getRtcRoom(feed.room)
	if !exist {
		log.Printf("listen(r): can't find rtcRoom, ignore")
		return
	}
	body := make(map[string]interface{})
	body["type"] = "offer"
	body["sdp"] = offer
	rawMsg := make(map[string]interface{})
	rawMsg["P-Asserted-Identity"] = r.local
	rawMsg["RouterMessage"] = true

	request := &rtclib.JSIP{
		Type:       rtclib.INVITE,
		RequestURI: r.remote,
		From:       feed.display,
		To:         rtcRoom,
		DialogueID: id,
		RawMsg:     rawMsg,
		Body:       body,
	}
	rtclib.SendMsg(request)

	candidates := getCandidatesFromSdp(offer)
	index := 0
	for _, candidate := range candidates {
		body := make(map[string]interface{})
		subBody := make(map[string]interface{})
		subBody["candidate"] = candidate
		subBody["sdpMLineIndex"] = index
		subBody["sdmMid"] = "sdp"
		body["candidate"] = subBody
		requestRawMsg := make(map[string]interface{})
		requestRawMsg["P-Asserted-Identity"] = r.local
		requestRawMsg["RouterMessage"] = true

		request := &rtclib.JSIP{
			Type:       rtclib.INFO,
			RequestURI: r.remote,
			From:       feed.display,
			To:         rtcRoom,
			DialogueID: id,
			Body:       body,
			RawMsg:     requestRawMsg,
		}
		rtclib.SendMsg(request)
	}
	if len(candidates) > 0 {
		body := make(map[string]interface{})
		subBody := make(map[string]interface{})
		subBody["completed"] = true
		body["candidate"] = subBody
		requestRawMsg := make(map[string]interface{})
		requestRawMsg["P-Asserted-Identity"] = r.local
		requestRawMsg["RouterMessage"] = true

		request := &rtclib.JSIP{
			Type:       rtclib.INFO,
			RequestURI: r.remote,
			From:       feed.display,
			To:         rtcRoom,
			DialogueID: id,
			Body:       body,
			RawMsg:     requestRawMsg,
		}
		rtclib.SendMsg(request)
	}

	listener := routerLis{
		rou:        r,
		hid:        hid,
		feed:       feed,
		dialogueID: id,
		rtcRoom:    rtcRoom,
	}

	for {
		select {
		case <-r.close:
			return
		case msg := <-msgChan:
			if !listener.messageHandle(msg) {
				log.Printf("listen(r): process `%d` quit", hid)
				return
			}
		}
	}
}

func (l *routerLis) messageHandle(msg message) bool {
	r := l.rou
	switch msg.desc {
	case "jsip":
		jsip := msg.content.(*rtclib.JSIP)
		switch jsip.Type {
		case rtclib.INVITE:
			if jsip.Code == 0 || jsip.Body == nil {
				log.Printf("listen(r): unexpected INVITE msg `%+v`", jsip)
				return false
			}
			answer, err := jsip.Body.(*simplejson.Json).Get("sdp").String()
			if err != nil {
				log.Printf("listen(r): no sdp in INVITE response `%+v`", jsip)
				return false
			}
			start(r.janusConn, r.sid, l.hid, l.feed.room, answer)

			requestRawMsg := make(map[string]interface{})
			requestRawMsg["P-Asserted-Identity"] = r.local
			requestRawMsg["RouterMessage"] = true
			relatedID := json.Number(strconv.FormatUint(jsip.CSeq, 10))
			requestRawMsg["RelatedID"] = relatedID
			response := &rtclib.JSIP{
				Type:       rtclib.ACK,
				RequestURI: r.remote,
				From:       jsip.To,
				To:         jsip.From,
				DialogueID: jsip.DialogueID,
				RawMsg:     requestRawMsg,
			}
			rtclib.SendMsg(response)
			return true
		case rtclib.INFO:
			if jsip.Code != 0 {
				return true
			}
			if jsip.Body == nil {
				log.Printf("listen(r): unexpected INFO msg `%+v`", jsip)
				return false
			}
			body := jsip.Body.(*simplejson.Json)
			candidateJson, exist := body.CheckGet("candidate")
			if exist == false {
				/* Now the info only transport candidate */
				log.Printf("listen(r): not found candidate from INFO msg")
				return false
			}

			completed, err := candidateJson.Get("completed").Bool()
			if err == nil && completed == true {
				completeCandidate(r.janusConn, r.sid, l.hid)
			} else {
				date, _ := candidateJson.Get("candidate").String()
				mid, _ := candidateJson.Get("sdpMid").String()
				index, _ := candidateJson.Get("sdpMLineIndex").Uint64()
				candidate(r.janusConn, r.sid, l.hid, index, date, mid)
			}

			rawMsg := make(map[string]interface{})
			rawMsg["P-Asserted-Identity"] = r.local
			rawMsg["RouterMessage"] = true
			response := &rtclib.JSIP{
				Type:       rtclib.INFO,
				Code:       200,
				From:       jsip.To,
				To:         jsip.From,
				CSeq:       jsip.CSeq,
				DialogueID: jsip.DialogueID,
				RawMsg:     rawMsg,
			}

			rtclib.SendMsg(response)
			return true
		case rtclib.BYE:
			detach(r.janusConn, r.sid, l.hid)
			return false
		}
	case "notifyUnpublish":
		log.Printf("listen: handle `%d` recv unpublish", l.hid)
		detach(r.janusConn, r.sid, l.hid)

		rawMsg := make(map[string]interface{})
		rawMsg["P-Asserted-Identity"] = r.local
		rawMsg["RouterMessage"] = true
		request := &rtclib.JSIP{
			Type:       rtclib.BYE,
			RequestURI: r.remote,
			From:       l.feed.display,
			To:         l.rtcRoom,
			DialogueID: l.dialogueID,
			RawMsg:     rawMsg,
		}

		rtclib.SendMsg(request)
		return false
	}
	return false
}

func (r *router) notifyFeed(feed feed) {
	msg := message{desc: "notifyFeed", content: feed}
	go sendMessage(msg, r.msgChan)
}

func (r *router) notifyUnpublish(feed feed) {
	msg := message{desc: "notifyUnpublish", content: feed}
	go sendMessage(msg, r.msgChan)
}

func (r *router) registerPublisher(id string, feed feed) {
	publish := make(map[string]interface{})
	publish["id"] = id
	publish["feed"] = feed
	msg := message{desc: "registerPublisher", content: publish}
	go sendMessage(msg, r.msgChan)
}

func (r *router) notifyQuit(id string) {
	msg := message{desc: "notifyQuit", content: id}
	go sendMessage(msg, r.msgChan)
}

func (r *router) keepalive() {
	// Just send keepalive per 30 second
	timer := time.After(30 * time.Second)

	for {
		select {
		case <-timer:
			log.Printf("keepalive: router `%d` send a keepalive", r.sid)
			keepalive(r.janusConn, r.sid)
			timer = time.After(30 * time.Second)
		case <-r.close:
			log.Printf("keepalive: router `%d` closed", r.sid)
			return
		}
	}
}

func (r *router) finish() {
	log.Printf("router: router for server `%s` finished", r.remote)
	r.global.routers.Delete(r.remote)
	close(r.close)
}

func (r *router) getRoom(rtcRoom string) (uint64, bool) {
	return r.global.getRoom(rtcRoom)
}

func (r *router) getRtcRoom(room uint64) (string, bool) {
	return r.global.getRtcRoom(room)
}

func (r *router) newDialogueID() string {
	u1, _ := uuid.NewV4()
	return r.local + r.remote + u1.String()
}
