package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"github.com/judwhite/go-svc/svc"
	"github.com/youzan/ZanRedisDB/common"
	"github.com/youzan/ZanRedisDB/node"
	"github.com/youzan/ZanRedisDB/server"
)

var (
	flagSet        = flag.NewFlagSet("zanredisdb", flag.ExitOnError)
	configFilePath = flagSet.String("config", "", "the config file path to read")
	logAge         = flagSet.Int("logage", 0, "the max age (day) log will keep")
	showVersion    = flagSet.Bool("version", false, "print version string and exit")
)

type program struct {
	server *server.Server
}

func main() {
	defer log.Printf("main exit")
	defer common.FlushZapDefault()
	prg := &program{}
	if err := svc.Run(prg, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGINT); err != nil {
		log.Panic(err)
	}
}

func (p *program) Init(env svc.Environment) error {
	if env.IsWindowsService() {
		dir := filepath.Dir(os.Args[0])
		return os.Chdir(dir)
	}
	return nil
}

func (p *program) Start() error {
	flagSet.Parse(os.Args[1:])

	fmt.Println(common.VerString("ZanRedisDB"))
	if *showVersion {
		os.Exit(0)
	}
	var configFile server.ConfigFile
	configDir := filepath.Dir(*configFilePath)
	if *configFilePath != "" {
		d, err := ioutil.ReadFile(*configFilePath)
		if err != nil {
			return err
		}
		err = json.Unmarshal(d, &configFile)
		if err != nil {
			return err
		}
	}
	if configFile.ServerConf.DataDir == "" {
		tmpDir, err := ioutil.TempDir("", fmt.Sprintf("rocksdb-test-%d", time.Now().UnixNano()))
		if err != nil {
			return err
		}
		configFile.ServerConf.DataDir = tmpDir
	}

	serverConf := configFile.ServerConf
	common.SetZapRotateOptions(false, true, path.Join(serverConf.LogDir, "zankv.log"), 0, 0, *logAge)

	loadConf, _ := json.MarshalIndent(configFile, "", " ")
	fmt.Printf("loading with conf:%v\n", string(loadConf))
	bip := server.GetIPv4ForInterfaceName(serverConf.BroadcastInterface)
	if bip == "" || bip == "0.0.0.0" {
		return errors.New("broadcast ip can not be found")
	} else {
		serverConf.BroadcastAddr = bip
	}
	fmt.Printf("broadcast ip is :%v\n", bip)
	app, err := server.NewServer(serverConf)
	if err != nil {
		return err
	}
	for _, nsNodeConf := range serverConf.Namespaces {
		nsFile := path.Join(configDir, nsNodeConf.Name)
		d, err := ioutil.ReadFile(nsFile)
		if err != nil {
			return err
		}
		var nsConf node.NamespaceConfig
		err = json.Unmarshal(d, &nsConf)
		if err != nil {
			return err
		}
		if nsConf.Name != nsNodeConf.Name {
			return errors.New("namespace name not match the config file")
		}
		if nsConf.Replicator <= 0 {
			return errors.New("namespace replicator should be set")
		}

		id := nsNodeConf.LocalReplicaID
		clusterNodes := make(map[uint64]node.ReplicaInfo)
		for _, v := range nsConf.RaftGroupConf.SeedNodes {
			clusterNodes[v.ReplicaID] = v
		}
		app.InitKVNamespace(id, &nsConf, false)
	}
	app.Start()
	p.server = app
	return nil
}

func (p *program) Stop() error {
	if p.server != nil {
		p.server.Stop()
	}
	return nil
}
