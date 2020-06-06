package main

import (
	"log"
	"strconv"
	"sync"

	"github.com/go-ini/ini"
	redis "gopkg.in/redis.v5"
	"rtclib"
)

type globalCtx struct {
	rClient  *redis.Client
	routers  sync.Map
	handlers sync.Map
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

	return vr
}

func (vr *Videoroom) NewSLPCtx() interface{} {
	var routers sync.Map
	var handlers sync.Map
	rClient := redis.NewClient(
		&redis.Options{
			Addr:     vr.config.RedisAddr,
			Password: vr.config.RedisPassword,
			DB:       int(vr.config.RedisDB),
		})
	if _, err := rClient.Ping().Result(); err != nil {
		log.Println("Init Videoroom ctx: ping redis err: ", err)
	}

	ctx := &globalCtx{
		routers:  routers,
		handlers: handlers,
		rClient:  rClient,
	}
	log.Printf("Init Videoroom ctx: ctx = %+v", ctx)
	return ctx
}

func (vr *Videoroom) OnLoad(jsip *rtclib.JSIP) {
	vr.finish()
	return
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
	vr.ctx = vr.task.GetCtx().(*globalCtx)

	msg := message{desc: "jsip", content: jsip}

	id := jsip.DialogueID
	process, exist := vr.ctx.handlers.Load(id)
	if exist {
		sendMessage(msg, process.(handler).MsgChan())
		return
	}

	if jsip.Type != rtclib.INVITE || jsip.Code != 0 {
		log.Printf("videoroom: no process for id `%s`", id)
		return
	}

	PAI, exist := jsip.RawMsg["P-Asserted-Identity"]
	if !exist {
		log.Printf("videoroom: no P-Asserted-Identity in message, quit")
		vr.finish()
		return
	}
	user, _ := PAI.(string)

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
			vr.ctx.handlers.Store(id, router)
			sendMessage(msg, router.MsgChan())
			return
		}
		log.Printf("videoroom: msg is sent from router")
	}

	session := newSession(vr, user)
	if session == nil {
		log.Printf("videoroom: create session for user `%s` failed", user)
		vr.finish()
		return
	}
	vr.ctx.handlers.Store(id, session)

	session.id = jsip.RequestURI
	sendMessage(msg, session.MsgChan())
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
