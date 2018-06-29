//
//
// session.go

package main

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"time"

	simplejson "github.com/bitly/go-simplejson"
	"github.com/tidwall/gjson"
	"janus"
	"rtclib"
)

type session struct {
	vr      *Videoroom
	user    string
	id      string
	msgChan chan message
	close   chan bool
	process map[string](chan message)
	feeds   map[feed]string

	janusConn *janus.Janus
	sid       uint64
}

type publisher struct {
	sess       *session
	hid        uint64
	rtcRoom    string
	dialogueID string
}

type listener struct {
	sess       *session
	hid        uint64
	feed       feed
	dialogueID string
}

type feed struct {
	room    uint64
	pid     uint64
	display string
}

func newSession(vr *Videoroom, user string) *session {
	janusConn := janus.NewJanus(vr.config.JanusAddr)
	if janusConn == nil {
		return nil
	}

	sid := newJanusSession(janusConn)
	s := &session{
		vr:        vr,
		user:      user,
		msgChan:   make(chan message),
		close:     make(chan bool),
		process:   make(map[string](chan message)),
		feeds:     make(map[feed]string),
		janusConn: janusConn,
		sid:       sid,
	}
	go s.keepalive()
	go s.main()
	return s
}

func (s *session) MsgChan() chan message {
	return s.msgChan
}

func (s *session) finish() {
	log.Printf("session: session for user `%s` finished", s.user)
	close(s.close)
	s.vr.finish()
	destroy(s.janusConn, s.sid)
	s.janusConn.Close()
}

func (s *session) messageHandle(msg message) {
	switch msg.desc {
	case "jsip":
		jsip := msg.content.(*rtclib.JSIP)
		id := jsip.DialogueID
		msgChan, exist := s.process[id]
		// DEBUG.
		// for key := range s.process {
		// 	log.Printf("DEBUG: handle jsip: process: key:`%s`", key)
		// }
		if exist {
			sendMessage(msg, msgChan)
			return
		}

		if jsip.Type != rtclib.INVITE || jsip.Code != 0 {
			log.Printf("session: no process for id `%s`", id)
			return
		}

		msgChan = make(chan message)
		s.process[id] = msgChan
		log.Printf("session: add process `%d`:`%s`", s.sid, id)

		go s.publish(msgChan, id)
		sendMessage(msg, msgChan)
	case "notifyFeed":
		feed := msg.content.(feed)
		_, exist := s.feeds[feed]
		if exist {
			return
		}

		id := s.vr.newDialogueID()
		for {
			_, exist := s.process[id]
			if exist {
				log.Printf("session: id `%s` alreay exist", id)
				id = s.vr.newDialogueID()
			}
			break
		}
		msgChan := make(chan message)
		s.process[id] = msgChan
		s.vr.ctx.handlers.Store(id, s)
		log.Printf("session: add process `%d`:`%s`", s.sid, id)

		s.feeds[feed] = id
		go s.listen(msgChan, id, feed)
		// DEBUG.
		// for key := range s.process {
		// 	log.Printf("DEBUG: notifyFeed: process: key:`%s`", key)
		// }
	case "notifyUnpublish":
		unpub := msg.content.(feed)

		// Unpublish message don't have display value.
		id := ""
		for feed := range s.feeds {
			if feed.pid == unpub.pid && feed.room == unpub.room {
				id = s.feeds[feed]
				delete(s.feeds, feed)
				break
			}
		}

		if id != "" {
			msgChan := s.process[id]
			go sendMessage(msg, msgChan)
		}

		return
	case "registerPublisher":
		feed := msg.content.(feed)
		s.feeds[feed] = ""
	case "notifyQuit":
		id := msg.content.(string)
		delete(s.process, id)
		s.vr.ctx.handlers.Delete(id)
		log.Printf("session: delete process `%d`:`%s`", s.sid, id)
		if len(s.process) == 0 {
			log.Printf("session: all process is done")
			s.finish()
		}

		for key, value := range s.feeds {
			if value == id {
				delete(s.feeds, key)
				break
			}
		}
	}
}

func (s *session) janusMessage(msg []byte) {
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
				s.notifyFeed(feed)
			}
		} else if data.Get("unpublished").Exists() {
			feed := feed{
				room: data.Get("room").Uint(),
				pid:  data.Get("unpublished").Uint(),
			}
			s.notifyUnpublish(feed)
		}
	case "media":
		receiving := gjson.GetBytes(msg, "receiving").Bool()
		if receiving {
			return
		}
		log.Println("janusMessage: media receiving is false")
		fallthrough
	case "hangup":
		hid := gjson.GetBytes(msg, "sender").Uint()
		msg := message{desc: "notifyHangup", content: hid}
		for _, msgChan := range s.process {
			go sendMessage(msg, msgChan)
		}
	}
}

func (s *session) main() {
	janusSess, _ := s.janusConn.Session(s.sid)
	janusMsg := janusSess.DefaultMsgChan()

	for {
		select {
		case <-s.close:
			return
		case msg := <-s.msgChan:
			log.Printf("main: receive msg `%+v`", msg)
			s.messageHandle(msg)
			log.Printf("main: msg `%+v` process end", msg)
		case msg := <-janusMsg:
			s.janusMessage(msg)
		}
	}
}

func (s *session) keepalive() {
	// Just send keepalive per 30 second
	timer := time.After(30 * time.Second)

	for {
		select {
		case <-timer:
			log.Printf("keepalive: session `%d` send a keepalive", s.sid)
			keepalive(s.janusConn, s.sid)
			timer = time.After(30 * time.Second)
		case <-s.close:
			log.Printf("keepalive: session `%d` closed", s.sid)
			return
		}
	}
}

func (pub *publisher) messageHandle(msg message) bool {
	s := pub.sess
	switch msg.desc {
	case "jsip":
		jsip := msg.content.(*rtclib.JSIP)
		switch jsip.Type {
		case rtclib.INVITE:
			if jsip.Body == nil {
				log.Printf("publish: unexpected INVITE msg `%+v`", jsip)
				return false
			}
			pub.rtcRoom = jsip.To
			display := jsip.From
			offer, err := jsip.Body.(*simplejson.Json).Get("sdp").String()
			if err != nil {
				log.Printf("publish: no sdp in message `%+v`", jsip)
				return false
			}

			room := s.getRoom(pub.rtcRoom, s.sid, pub.hid)
			publishMsg := publish(s.janusConn, s.sid, pub.hid, room, display)
			if publishMsg == nil {
				return false
			}

			data := gjson.GetBytes(publishMsg, "plugindata.data")
			pid := data.Get("id").Uint()
			f := feed{room: room, pid: pid, display: display}
			s.registerPublisher(f)
			publishers := data.Get("publishers").Array()
			for _, pubMsg := range publishers {
				f := feed{
					room:    room,
					pid:     pubMsg.Get("id").Uint(),
					display: pubMsg.Get("display").String(),
				}
				s.notifyFeed(f)
			}

			answer := configure(s.janusConn, s.sid, pub.hid, offer)
			if answer == "" {
				return false
			}

			body := make(map[string]interface{})
			body["type"] = "answer"
			body["sdp"] = answer
			rawMsg := make(map[string]interface{})
			rawMsg["P-Asserted-Identity"] = s.id

			response := &rtclib.JSIP{
				Type:       rtclib.INVITE,
				Code:       200,
				From:       pub.rtcRoom,
				To:         s.user,
				CSeq:       jsip.CSeq,
				DialogueID: jsip.DialogueID,
				RawMsg:     rawMsg,
				Body:       body,
			}

			rtclib.SendMsg(response)
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
				completeCandidate(s.janusConn, s.sid, pub.hid)
			} else {
				value, _ := date.Get("candidate").String()
				mid, _ := date.Get("sdpMid").String()
				index, _ := date.Get("sdpMLineIndex").Uint64()
				candidate(s.janusConn, s.sid, pub.hid, index, value, mid)
			}

			rawMsg := make(map[string]interface{})
			rawMsg["P-Asserted-Identity"] = s.id
			response := &rtclib.JSIP{
				Type:       rtclib.INFO,
				Code:       200,
				From:       pub.rtcRoom,
				To:         s.user,
				CSeq:       jsip.CSeq,
				DialogueID: jsip.DialogueID,
				RawMsg:     rawMsg,
			}
			rtclib.SendMsg(response)
			return true
		case rtclib.BYE:
			unpublish(s.janusConn, s.sid, pub.hid)
			detach(s.janusConn, s.sid, pub.hid)

			// Delete room info in redis.
			s.vr.ctx.decrMember(pub.rtcRoom)
			s.deregisterRoom(pub.rtcRoom)
			return false
		}
	case "notifyHangup":
		hangId := msg.content.(uint64)
		if pub.hid != hangId {
			return true
		}

		log.Printf("publish: handle `%d` hangup", pub.hid)
		unpublish(s.janusConn, s.sid, pub.hid)
		detach(s.janusConn, s.sid, pub.hid)

		s.vr.ctx.decrMember(pub.rtcRoom)
		s.deregisterRoom(pub.rtcRoom)

		requestRawMsg := make(map[string]interface{})
		requestRawMsg["P-Asserted-Identity"] = s.id
		request := &rtclib.JSIP{
			Type:       rtclib.BYE,
			RequestURI: s.user,
			From:       pub.rtcRoom,
			To:         s.user,
			DialogueID: pub.dialogueID,
			RawMsg:     requestRawMsg,
		}

		rtclib.SendMsg(request)
		return false
	}
	return true
}

func (s *session) publish(msgChan chan message, id string) {
	defer s.notifyQuit(id)

	hid := attachVideoroom(s.janusConn, s.sid)
	pub := publisher{
		sess:       s,
		hid:        hid,
		dialogueID: id,
	}

	for {
		select {
		case <-s.close:
			return
		case msg := <-msgChan:
			if !pub.messageHandle(msg) {
				log.Printf("publish: process `%d` quit", hid)
				return
			}
		}
	}
}

func (l *listener) messageHandle(msg message) bool {
	s := l.sess
	switch msg.desc {
	case "jsip":
		jsip := msg.content.(*rtclib.JSIP)
		switch jsip.Type {
		case rtclib.INVITE:
			if jsip.Code == 0 || jsip.Body == nil {
				log.Printf("listen: unexpected INVITE msg `%+v`", jsip)
				return false
			}
			answer, err := jsip.Body.(*simplejson.Json).Get("sdp").String()
			if err != nil {
				log.Printf("listen: no sdp in INVITE response `%+v`", jsip)
				return false
			}
			start(s.janusConn, s.sid, l.hid, l.feed.room, answer)

			response := &rtclib.JSIP{
				Type:       rtclib.ACK,
				RequestURI: s.user,
				From:       l.feed.display,
				To:         s.user,
				DialogueID: l.dialogueID,
				RawMsg:     make(map[string]interface{}),
			}
			relatedID := json.Number(strconv.FormatUint(jsip.CSeq, 10))
			response.RawMsg["RelatedID"] = relatedID
			response.RawMsg["P-Asserted-Identity"] = s.id

			rtclib.SendMsg(response)
			return true
		case rtclib.INFO:
			if jsip.Code != 0 {
				return true
			}
			if jsip.Body == nil {
				log.Printf("listen: unexpected INFO msg `%+v`", jsip)
				return false
			}
			body := jsip.Body.(*simplejson.Json)
			candidateJson, exist := body.CheckGet("candidate")
			if exist == false {
				/* Now the info only transport candidate */
				log.Printf("listen: not found candidate from INFO msg")
				return false
			}

			completed, err := candidateJson.Get("completed").Bool()
			if err == nil && completed == true {
				completeCandidate(s.janusConn, s.sid, l.hid)
			} else {
				date, _ := candidateJson.Get("candidate").String()
				mid, _ := candidateJson.Get("sdpMid").String()
				index, _ := candidateJson.Get("sdpMLineIndex").Uint64()
				candidate(s.janusConn, s.sid, l.hid, index, date, mid)
			}

			rawMsg := make(map[string]interface{})
			rawMsg["P-Asserted-Identity"] = s.id
			response := &rtclib.JSIP{
				Type:       rtclib.INFO,
				Code:       200,
				From:       jsip.To,
				To:         s.user,
				CSeq:       jsip.CSeq,
				DialogueID: jsip.DialogueID,
				RawMsg:     rawMsg,
			}
			rtclib.SendMsg(response)
			return true
		case rtclib.BYE:
			detach(s.janusConn, s.sid, l.hid)
			return false
		}
	case "notifyUnpublish":
		log.Printf("listen: handle `%d` recv unpublish", l.hid)
		detach(s.janusConn, s.sid, l.hid)

		requestRawMsg := make(map[string]interface{})
		requestRawMsg["P-Asserted-Identity"] = s.id
		request := &rtclib.JSIP{
			Type:       rtclib.BYE,
			RequestURI: s.user,
			From:       l.feed.display,
			To:         s.user,
			DialogueID: l.dialogueID,
			RawMsg:     requestRawMsg,
		}

		rtclib.SendMsg(request)
		return false
	case "notifyHangup":
		hangId := msg.content.(uint64)
		if l.hid != hangId {
			return true
		}

		log.Printf("listen: handle `%d` hangup", l.hid)
		detach(s.janusConn, s.sid, l.hid)

		requestRawMsg := make(map[string]interface{})
		requestRawMsg["P-Asserted-Identity"] = s.id
		request := &rtclib.JSIP{
			Type:       rtclib.BYE,
			RequestURI: s.user,
			From:       l.feed.display,
			To:         s.user,
			DialogueID: l.dialogueID,
			RawMsg:     requestRawMsg,
		}

		rtclib.SendMsg(request)
		return false
	}
	return true
}

func (s *session) listen(msgChan chan message, id string, feed feed) {
	defer s.notifyQuit(id)

	pubHid := attachVideoroom(s.janusConn, s.sid)
	// Need private_id, so must join as publisher first.
	joinMsg := publish(s.janusConn, s.sid, pubHid, feed.room, feed.display)
	data := gjson.GetBytes(joinMsg, "plugindata.data")
	privateID := data.Get("private_id").Uint()

	hid := attachVideoroom(s.janusConn, s.sid)
	offer := listen(s.janusConn, s.sid, hid, feed.room, feed.pid, privateID)

	rtcRoom, exist := s.vr.ctx.getRtcRoom(feed.room)
	if !exist {
		log.Printf("listen: can't find rtcRoom, close session")
		s.finish()
		return
	}
	body := make(map[string]interface{})
	body["type"] = "offer"
	body["sdp"] = offer
	body["room"] = rtcRoom
	rawMsg := make(map[string]interface{})
	rawMsg["P-Asserted-Identity"] = s.id

	request := &rtclib.JSIP{
		Type:       rtclib.INVITE,
		RequestURI: s.user,
		From:       feed.display,
		To:         s.user,
		DialogueID: id,
		RawMsg:     rawMsg,
		Body:       body,
	}
	rtclib.SendMsg(request)

	listener := listener{
		sess:       s,
		hid:        hid,
		feed:       feed,
		dialogueID: id,
	}

	for {
		select {
		case <-s.close:
			return
		case msg := <-msgChan:
			if !listener.messageHandle(msg) {
				log.Printf("listen: process `%d` quit", hid)
				return
			}
		}
	}
}

func (s *session) getRoom(rtcRoom string, sid uint64, hid uint64) uint64 {
	room, exist := s.vr.ctx.getRoom(rtcRoom)
	if exist {
		s.vr.ctx.incrMember(rtcRoom)
		go s.registerRoom(rtcRoom, room)
		return room
	}

	room = createRoom(s.janusConn, sid, hid)
	s.vr.ctx.setRoom(rtcRoom, room)
	go s.registerRoom(rtcRoom, room)
	log.Printf("getRoom: add janusRoom `%d` with rtcRoom `%s", room, rtcRoom)
	return room
}

func (s *session) registerRoom(id string, room uint64) {
	msg := make(map[string]interface{})
	msg["janus"] = s.vr.config.JanusAddr
	msg["server"] = rtclib.Realm()
	msg["room"] = room

	b, err := json.Marshal(msg)
	if err != nil {
		log.Println("registerRoom: json err: ", err)
		return
	}

	addr := s.vr.config.ApiAddr + "/register/" + id
	contentType := "application/json;charset=utf-8"
	body := bytes.NewBuffer(b)

	resp, err := http.Post(addr, contentType, body)
	if err != nil {
		log.Println("registerRoom: post err: ", err)
		return
	}

	defer resp.Body.Close()
	rBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println("registerRoom: read err: ", err)
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("registerRoom: register failed, response code: `%d`, "+
			"body: `%s`", resp.StatusCode, rBody)
		return
	}

	if gjson.GetBytes(rBody, "register").Bool() == true {
		return
	}

	remote := gjson.GetBytes(rBody, "server").String()
	s.route(remote, room)
}

func (s *session) deregisterRoom(rtcRoom string) {
	msg := make(map[string]interface{})
	msg["server"] = rtclib.Realm()

	b, err := json.Marshal(msg)
	if err != nil {
		log.Println("deregisterRoom: json err", err)
		return
	}
	body := bytes.NewBuffer(b)
	addr := s.vr.config.ApiAddr + "/deregister/" + rtcRoom
	contentType := "application/json;charset=utf-8"

	resp, err := http.Post(addr, contentType, body)
	if err != nil {
		log.Println("deregisterRoom: post err: ", err)
		return
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("deregisterRoom: deregister failed, response code: `%d`",
			resp.StatusCode)
	}

	return
}

func (s *session) route(remote string, room uint64) {
	var rou *router
	cached, exist := s.vr.ctx.routers.Load(remote)
	if !exist {
		conn := janus.NewJanus(s.vr.config.JanusAddr)
		if conn == nil {
			log.Printf("route: can't connect to janus, quit")
		}
		rou = newRouter(remote, conn, s.vr.ctx)
		s.vr.ctx.routers.Store(remote, rou)
	} else {
		rou = cached.(*router)
	}

	rou.new(room)
}

func (s *session) notifyFeed(feed feed) {
	msg := message{desc: "notifyFeed", content: feed}
	go sendMessage(msg, s.msgChan)
}

func (s *session) notifyUnpublish(feed feed) {
	msg := message{desc: "notifyUnpublish", content: feed}
	go sendMessage(msg, s.msgChan)
}

func (s *session) registerPublisher(feed feed) {
	msg := message{desc: "registerPublisher", content: feed}
	go sendMessage(msg, s.msgChan)
}

func (s *session) notifyQuit(id string) {
	msg := message{desc: "notifyQuit", content: id}
	go sendMessage(msg, s.msgChan)
}
