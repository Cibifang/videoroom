package main

import (
	"log"
	"strconv"
	"sync"

	"github.com/go-ini/ini"
	"github.com/go-redis/redis"
	"rtclib"
)

type globalCtx struct {
	rClient  *redis.Client
	sessions sync.Map
	routers  sync.Map
}

type Config struct {
	JanusAddr     string
	RedisAddr     string
	RedisPassword string
	RedisDB       uint64
	ApiAddr       string
}

type Videoroom struct {
	task   *rtclib.Task
	config *Config
	ctx    *globalCtx
}

func GetInstance(task *rtclib.Task) rtclib.SLP {
	var vr = &Videoroom{
		task: task,
	}
	if !vr.loadConfig() {
		log.Println("Videoroom load config failed")
	}

	if task.Ctx.Body == nil {
		var sessions sync.Map
		var routers sync.Map
		ctx := &globalCtx{
			sessions: sessions,
			routers:  routers,
		}

		ctx.rClient = redis.NewClient(
			&redis.Options{
				Addr:     vr.config.RedisAddr,
				Password: vr.config.RedisPassword,
				DB:       int(vr.config.RedisDB),
			})
		pong, err := ctx.rClient.Ping().Result()
		if err != nil {
			log.Println("Init videoroom: ping redis err: ", err)
		}
		log.Println("Init videoroom: ping redis, response: ", pong)

		task.Ctx.Body = ctx
		log.Printf("Init videoroom ctx, ctx = %+v", task.Ctx.Body)
	}

	vr.ctx = task.Ctx.Body.(*globalCtx)
	return vr
}

func (vr *Videoroom) loadConfig() bool {
	vr.config = new(Config)

	confPath := rtclib.RTCPATH + "/conf/Videoroom.ini"

	f, err := ini.Load(confPath)
	if err != nil {
		log.Printf("Load config file %s error: %v", confPath, err)
		return false
	}

	return rtclib.Config(f, "Videoroom", vr.config)
}

func (vr *Videoroom) Process(jsip *rtclib.JSIP) {
	log.Printf("videoroom: recv msg: %+v", jsip)
	log.Printf("videoroom: The config: %+v", vr.config)

	PAI, exist := jsip.RawMsg["P-Asserted-Identity"]
	if !exist {
		log.Printf("videoroom: no P-Asserted-Identity in message, ignore")
		return
	}
	user, _ := PAI.(string)

	msg := message{desc: "jsip", content: jsip}

	if jsip.RequestURI == rtclib.Realm() {
		fromRouter, exist := jsip.RawMsg["RouterMessage"]
		if !exist || !fromRouter.(bool) {
			log.Printf("videoroom: msg is sent to router")
			cachedRouter, exist := vr.ctx.routers.Load(user)
			if !exist {
				log.Printf("videoroom: not found router `%s`", user)
				return
			}
			router := cachedRouter.(*router)
			sendMessage(msg, router.msgChan)
			return
		}
		log.Printf("videoroom: msg is sent from router")
	}

	session, exist := vr.cachedSession(user)
	if exist {
		sendMessage(msg, session.msgChan)
		return
	}

	if jsip.Type != rtclib.INVITE || jsip.Code != 0 {
		log.Printf("videoroom: no session for user `%s`", user)
	}

	vr.newSession(user)
	session, exist = vr.cachedSession(user)
	if !exist {
		log.Printf("videoroom: create session for user `%s` failed", user)
		vr.finish()
		return
	}

	session.id = jsip.RequestURI
	sendMessage(msg, session.msgChan)
}

func (vr *Videoroom) newSession(user string) {
	session := newSession(vr, user)
	if session == nil {
		return
	}

	_, ok := vr.ctx.sessions.LoadOrStore(user, session)
	if !ok {
		log.Printf("videoroom: session for user `%s` is rewrited", user)
	}
}

func (vr *Videoroom) cachedSession(user string) (*session, bool) {
	s, exist := vr.ctx.sessions.Load(user)
	if !exist {
		return nil, exist
	}
	return s.(*session), exist
}

func (vr *Videoroom) deleteSession(user string) {
	vr.ctx.sessions.Delete(user)
}

func (vr *Videoroom) finish() {
	vr.task.SetFinished()
}

func (ctx *globalCtx) getRoom(rtcRoom string) (uint64, bool) {
	room, err := ctx.rClient.HGet("room", rtcRoom).Uint64()
	if err == redis.Nil {
		log.Printf("getRoom: room %s isn't existed", rtcRoom)
	} else if err != nil {
		log.Printf("getRoom: redis err: %s", err.Error())
	} else {
		return room, true
	}

	return room, false
}

func (ctx *globalCtx) getRtcRoom(room uint64) (string, bool) {
	roomStr := strconv.FormatUint(uint64(room), 10)
	rtcRoom, err := ctx.rClient.HGet("rtcRoom", roomStr).Result()
	if err == redis.Nil {
		log.Printf("getRtcRoom: room %d isn't existed", room)
	} else if err != nil {
		log.Printf("getRtcRoom: redis err: %s", err.Error())
	} else {
		return rtcRoom, true
	}

	return rtcRoom, false
}

func (ctx *globalCtx) setRoom(rtcRoom string, room uint64) {
	err := ctx.rClient.HSet("room", rtcRoom, room).Err()
	if err != nil {
		log.Printf("setRoom: redis err: %s", err.Error())
		return
	}

	roomStr := strconv.FormatUint(uint64(room), 10)
	err = ctx.rClient.HSet("rtcRoom", roomStr, rtcRoom).Err()
	if err != nil {
		log.Printf("setRoom: redis err: %s", err.Error())
		return
	}

	ctx.incrMember(rtcRoom)
}

func (ctx *globalCtx) delRoom(rtcRoom string) {
	janusRoom, exist := ctx.getRoom(rtcRoom)
	if !exist {
		log.Printf("delRoom: no room `%s`", rtcRoom)
		return
	}

	_, err := ctx.rClient.HDel("room", rtcRoom).Result()
	if err != nil {
		log.Printf("delRoom: redis err: %s", err.Error())
		return
	}

	roomStr := strconv.FormatUint(uint64(janusRoom), 10)
	_, err = ctx.rClient.HDel("rtcRoom", roomStr).Result()
	if err != nil {
		log.Printf("delRoom: redis err: %s", err.Error())
		return
	}

	_, err = ctx.rClient.HDel("member", rtcRoom).Result()
	if err != nil {
		log.Printf("delRoom: redis err: %s", err.Error())
		return
	}

	log.Printf("delRoom: delete room `%d`:`%s`", janusRoom, rtcRoom)
}

func (ctx *globalCtx) incrMember(rtcRoom string) {
	err := ctx.rClient.HIncrBy("member", rtcRoom, 1).Err()
	if err != nil {
		log.Printf("incrMember: redis err: %s", err.Error())
	}
}

func (ctx *globalCtx) decrMember(room string) {
	num, err := ctx.rClient.HIncrBy("member", room, -1).Result()
	if err != nil {
		log.Printf("decrMember: redis err: %s", err.Error())
		return
	}

	if num <= 0 {
		log.Printf("decrMember: room %s don't have any member, delete it", room)
		ctx.delRoom(room)
	}
}

func (vr *Videoroom) newDialogueID() string {
	return vr.task.NewDialogueID()
}
