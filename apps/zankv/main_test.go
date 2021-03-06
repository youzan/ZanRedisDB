package main

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/youzan/ZanRedisDB/server"
)

func TestAppConfigParse(t *testing.T) {
	flagSet.Parse([]string{})

	var configFile server.ConfigFile
	d, err := ioutil.ReadFile("../../default.conf")
	assert.Nil(t, err)
	err = json.Unmarshal(d, &configFile)
	assert.Nil(t, err)

	serverConf := configFile.ServerConf
	serverConf.LogDir = path.Join(os.TempDir(), strconv.Itoa(int(time.Now().UnixNano())))
	if runtime.GOOS == "darwin" {
		serverConf.BroadcastInterface = "lo0"
	} else {
		serverConf.BroadcastInterface = "lo"
	}
	_, err = server.NewServer(serverConf)
	assert.Nil(t, err)
}
