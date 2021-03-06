package server

import (
	"encoding/json"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/absolute8511/redcon"
	"github.com/youzan/ZanRedisDB/common"
)

var (
	errInvalidCommand = common.ErrInvalidCommand
	costStatsLevel    int32
)

// TODO: maybe provide reusable memory buffer for req and response
func (s *Server) serverRedis(conn redcon.Conn, cmd redcon.Command) {
	defer func() {
		if e := recover(); e != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			buf = buf[0:n]
			sLog.Infof("handle redis command %v panic: %s:%v", string(cmd.Args[0]), buf, e)
			conn.Close()
		}
	}()

	_, cmd, err := pipelineCommand(conn, cmd)
	if err != nil {
		conn.WriteError("pipeline error '" + err.Error() + "'")
		return
	}
	cmdName := qcmdlower(cmd.Args[0])
	switch cmdName {
	case "detach":
		hconn := conn.Detach()
		sLog.Infof("connection has been detached")
		go func() {
			defer hconn.Close()
			hconn.WriteString("OK")
			hconn.Flush()
		}()
	case "ping":
		conn.WriteString("PONG")
	case "auth":
		conn.WriteString("OK")
	case "quit":
		conn.WriteString("OK")
		conn.Close()
	case "info":
		s := s.GetStats(false, false)
		d, _ := json.MarshalIndent(s, "", " ")
		conn.WriteBulkString(string(d))
	default:
		if common.IsMergeCommand(cmdName) {
			s.doMergeCommand(conn, cmd)
		} else {
			var start time.Time
			level := atomic.LoadInt32(&costStatsLevel)
			if level > 0 {
				start = time.Now()
			}
			cmdStr := string(cmd.Args[0])
			ns, pk, pkSum, err := GetPKAndHashSum(cmdName, cmd)
			if err != nil {
				conn.WriteError(err.Error() + " : ERR handle command " + cmdStr)
				break
			}
			if len(cmd.Args) > 1 {
				cmdStr += ", " + string(cmd.Args[1])
				if level > 4 && len(cmd.Args) > 2 {
					for _, arg := range cmd.Args[2:] {
						cmdStr += "," + string(arg)
					}
				}
			}
			kvn, err := s.GetHandleNode(ns, pk, pkSum, cmdName, cmd)
			if err == nil {
				err = s.handleRedisSingleCmd(cmdName, pk, pkSum, kvn, conn, cmd)
			}
			if err != nil {
				conn.WriteError(err.Error() + " : ERR handle command " + cmdStr)
			}
			if level > 0 && err == nil {
				cost := time.Since(start)
				if cost >= time.Second ||
					(level > 1 && cost > time.Millisecond*500) ||
					(level > 2 && cost > time.Millisecond*100) ||
					(level > 3 && cost > time.Millisecond) ||
					(level > 4) {
					sLog.Infof("slow command %v cost %v", cmdStr, cost)
				}
			}
		}
	}
}

func (s *Server) serveRedisAPI(port int, stopC <-chan struct{}) {
	redisS := redcon.NewServer(
		":"+strconv.Itoa(port),
		s.serverRedis,
		func(conn redcon.Conn) bool {
			//sLog.Infof("accept: %s", conn.RemoteAddr())
			return true
		},
		func(conn redcon.Conn, err error) {
			if err != nil {
				sLog.Infof("closed: %s, err: %v", conn.RemoteAddr(), err)
			}
		},
	)
	redisS.SetIdleClose(time.Minute * 5)
	go func() {
		err := redisS.ListenAndServe()
		if err != nil {
			sLog.Fatalf("failed to start the redis server: %v", err)
		}
	}()
	<-stopC
	redisS.Close()
	sLog.Infof("redis api server exit\n")
}
