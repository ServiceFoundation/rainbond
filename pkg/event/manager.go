// RAINBOND, Application Management Platform
// Copyright (C) 2014-2017 Goodrain Co., Ltd.

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. For any non-GPL usage of Rainbond,
// one or multiple Commercial Licenses authorized by Goodrain Co., Ltd.
// must be obtained first.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package event

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/goodrain/rainbond/pkg/discover"
	"github.com/goodrain/rainbond/pkg/discover/config"
	"github.com/goodrain/rainbond/pkg/util"

	"github.com/pquerna/ffjson/ffjson"

	"github.com/Sirupsen/logrus"

	eventclient "github.com/goodrain/rainbond/pkg/eventlog/entry/grpc/client"
	eventpb "github.com/goodrain/rainbond/pkg/eventlog/entry/grpc/pb"

	"golang.org/x/net/context"
)

//Manager 操作日志，客户端服务
//客户端负载均衡
type Manager interface {
	GetLogger(eventID string) Logger
	Start() error
	Close() error
	ReleaseLogger(Logger)
}
type EventConfig struct {
	EventLogServers []string
	DiscoverAddress []string
}
type manager struct {
	ctx            context.Context
	cancel         context.CancelFunc
	config         EventConfig
	qos            int32
	loggers        map[string]Logger
	handles        map[string]handle
	lock           sync.Mutex
	eventServer    []string
	abnormalServer map[string]string
	dis            discover.Discover
}

var defaultManager Manager

const (
	//REQUESTTIMEOUT  time out
	REQUESTTIMEOUT = 1000 * time.Millisecond
	//MAXRETRIES 重试
	MAXRETRIES = 3 //  Before we abandon
)

//NewManager 创建manager
func NewManager(conf EventConfig) error {
	dis, err := discover.GetDiscover(config.DiscoverConfig{EtcdClusterEndpoints: conf.DiscoverAddress})
	if err != nil {
		logrus.Error("create discover manager error.", err.Error())
		if len(conf.EventLogServers) < 1 {
			return err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defaultManager = &manager{
		ctx:            ctx,
		cancel:         cancel,
		config:         conf,
		loggers:        make(map[string]Logger, 1024),
		handles:        make(map[string]handle),
		eventServer:    conf.EventLogServers,
		dis:            dis,
		abnormalServer: make(map[string]string),
	}
	return defaultManager.Start()
}

//GetManager 获取日志服务
func GetManager() Manager {
	return defaultManager
}

//CloseManager 关闭日志服务
func CloseManager() {
	if defaultManager != nil {
		defaultManager.Close()
	}
}

func (m *manager) Start() error {
	m.lock.Lock()
	defer m.lock.Unlock()
	for i := 0; i < len(m.eventServer); i++ {
		h := handle{
			cacheChan: make(chan []byte, 100),
			stop:      make(chan struct{}),
			server:    m.eventServer[i],
			manager:   m,
			ctx:       m.ctx,
		}
		m.handles[m.eventServer[i]] = h
		go h.HandleLog()
	}
	if m.dis != nil {
		m.dis.AddProject("event_log_event_grpc", m)
	}
	go m.GC()
	return nil
}

func (m *manager) UpdateEndpoints(endpoints ...*config.Endpoint) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if endpoints == nil || len(endpoints) < 1 {
		return
	}
	logrus.Infof("Update event server endpoint,%+v", endpoints)
	//清空不可用节点信息，以服务发现为主
	m.abnormalServer = make(map[string]string)
	//增加新节点
	var new = make(map[string]string)
	for _, end := range endpoints {
		new[end.URL] = end.URL
		if _, ok := m.handles[end.URL]; !ok {
			h := handle{
				cacheChan: make(chan []byte, 100),
				stop:      make(chan struct{}),
				server:    end.URL,
				manager:   m,
				ctx:       m.ctx,
			}
			m.handles[end.URL] = h
			go h.HandleLog()
		}
	}
	//删除旧节点
	for k := range m.handles {
		if _, ok := new[k]; !ok {
			delete(m.handles, k)
		}
	}
	var eventServer []string
	for k := range new {
		eventServer = append(eventServer, k)
	}
	m.eventServer = eventServer
	logrus.Infof("update event handle core success,handle core count:%d, event server count:%d", len(m.handles), len(m.eventServer))

}

func (m *manager) Error(err error) {

}
func (m *manager) Close() error {
	m.cancel()
	m.dis.Stop()
	return nil
}

func (m *manager) GC() {
	util.IntermittentExec(m.ctx, func() {
		m.lock.Lock()
		defer m.lock.Unlock()
		var needRelease []string
		for k, l := range m.loggers {
			//1min 未release ,自动gc
			if l.CreateTime().Add(time.Minute).Before(time.Now()) {
				needRelease = append(needRelease, k)
			}
		}
		if len(needRelease) > 0 {
			for _, event := range needRelease {
				logrus.Infof("start auto release event logger. %s", event)
				delete(m.loggers, event)
			}
		}
	}, time.Second*20)
}

//GetLogger
//使用完成后必须调用ReleaseLogger方法
func (m *manager) GetLogger(eventID string) Logger {
	m.lock.Lock()
	defer m.lock.Unlock()
	if eventID == " " || len(eventID) == 0 {
		eventID = "system"
	}
	if l, ok := m.loggers[eventID]; ok {
		return l
	}
	l := &logger{
		event:      eventID,
		sendChan:   m.getLBChan(),
		createTime: time.Now(),
	}
	m.loggers[eventID] = l
	return l
}

func (m *manager) ReleaseLogger(l Logger) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if l, ok := m.loggers[l.Event()]; ok {
		delete(m.loggers, l.Event())
	}
}

type handle struct {
	server    string
	stop      chan struct{}
	cacheChan chan []byte
	ctx       context.Context
	manager   *manager
}

func (m *manager) DiscardedLoggerChan(cacheChan chan []byte) {
	m.lock.Lock()
	defer m.lock.Unlock()
	for k, v := range m.handles {
		if v.cacheChan == cacheChan {
			logrus.Warnf("event server %s can not link, will ignore it.", k)
			m.abnormalServer[k] = k
		}
	}
	for _, v := range m.loggers {
		if v.GetChan() == cacheChan {
			v.SetChan(m.getLBChan())
		}
	}
}

func (m *manager) getLBChan() chan []byte {
	for i := 0; i < len(m.eventServer); i++ {
		index := m.qos % int32(len(m.eventServer))
		m.qos = atomic.AddInt32(&(m.qos), 1)
		server := m.eventServer[index]
		if _, ok := m.abnormalServer[server]; ok {
			continue
		}
		if h, ok := m.handles[server]; ok {
			return h.cacheChan
		}
		h := handle{
			cacheChan: make(chan []byte, 100),
			stop:      make(chan struct{}),
			server:    server,
			manager:   m,
			ctx:       m.ctx,
		}
		m.handles[server] = h
		go h.HandleLog()
		return h.cacheChan
	}
	//实在选不出节点了，返回列表第一个
	for _, v := range m.handles {
		return v.cacheChan
	}
	//列表不存在，返回nil
	return nil
}
func (m *manager) RemoveHandle(server string) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if _, ok := m.handles[server]; ok {
		delete(m.handles, server)
	}
}
func (m *handle) HandleLog() error {
	defer m.manager.RemoveHandle(m.server)
	return util.Exec(m.ctx, func() error {
		ctx, cancel := context.WithCancel(m.ctx)
		defer cancel()
		client, err := eventclient.NewEventClient(ctx, m.server)
		if err != nil {
			logrus.Error("create event client error.", err.Error())
			return err
		}
		logrus.Infof("start a event log handle core. connect server %s", m.server)
		logClient, err := client.Log(ctx)
		if err != nil {
			logrus.Error("create event log client error.", err.Error())
			//切换使用此chan的logger到其他chan
			m.manager.DiscardedLoggerChan(m.cacheChan)
			return err
		}
		for {
			select {
			case <-m.ctx.Done():
				logClient.CloseSend()
				return nil
			case <-m.stop:
				logClient.CloseSend()
				return nil
			case me := <-m.cacheChan:
				err := logClient.Send(&eventpb.LogMessage{Log: me})
				if err != nil {
					logrus.Error("send event log error.", err.Error())
					logClient.CloseSend()
					//切换使用此chan的logger到其他chan
					m.manager.DiscardedLoggerChan(m.cacheChan)
					return nil
				}
			}
		}
	}, time.Second*3)
}

func (m *handle) Stop() {
	close(m.stop)
}

//Logger 日志发送器
type Logger interface {
	Info(string, map[string]string)
	Error(string, map[string]string)
	Debug(string, map[string]string)
	Event() string
	CreateTime() time.Time
	GetChan() chan []byte
	SetChan(chan []byte)
}

type logger struct {
	event      string
	sendChan   chan []byte
	createTime time.Time
}

func (l *logger) GetChan() chan []byte {
	return l.sendChan
}
func (l *logger) SetChan(ch chan []byte) {
	l.sendChan = ch
}
func (l *logger) Event() string {
	return l.event
}
func (l *logger) CreateTime() time.Time {
	return l.createTime
}
func (l *logger) Info(message string, info map[string]string) {
	if info == nil {
		info = make(map[string]string)
	}
	info["level"] = "info"
	l.send(message, info)
}
func (l *logger) Error(message string, info map[string]string) {
	if info == nil {
		info = make(map[string]string)
	}
	info["level"] = "error"
	l.send(message, info)
}
func (l *logger) Debug(message string, info map[string]string) {
	if info == nil {
		info = make(map[string]string)
	}
	info["level"] = "debug"
	l.send(message, info)
}
func (l *logger) send(message string, info map[string]string) {
	info["event_id"] = l.event
	info["message"] = message
	info["time"] = time.Now().Format(time.RFC3339)
	log, err := ffjson.Marshal(info)
	if err == nil && l.sendChan != nil {
		util.SendNoBlocking(log, l.sendChan)
	}
}
