package main

import (
	"errors"
	"fmt"
	jsoniter "github.com/json-iterator/go"
	jsonIteratorExtra "github.com/json-iterator/go/extra"
	"github.com/tidwall/gjson"
	"github.com/v2rayA/v2rayA/common/netTools/ports"
	"github.com/v2rayA/v2rayA/common/resolv"
	"github.com/v2rayA/v2rayA/conf"
	"github.com/v2rayA/v2rayA/core/serverObj"
	"github.com/v2rayA/v2rayA/core/v2ray"
	"github.com/v2rayA/v2rayA/core/v2ray/asset"
	"github.com/v2rayA/v2rayA/core/v2ray/asset/gfwlist"
	service2 "github.com/v2rayA/v2rayA/core/v2ray/service"
	"github.com/v2rayA/v2rayA/core/v2ray/where"
	"github.com/v2rayA/v2rayA/db"
	"github.com/v2rayA/v2rayA/db/configure"
	"github.com/v2rayA/v2rayA/pkg/util/gopeed"
	"github.com/v2rayA/v2rayA/pkg/util/log"
	"github.com/v2rayA/v2rayA/server/router"
	"github.com/v2rayA/v2rayA/server/service"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"runtime"
	"sync"
	"syscall"
	"time"
)

func checkEnvironment() {
	config := conf.GetEnvironmentConfig()
	if len(config.PrintReport) > 0 {
		config.Report()
		os.Exit(0)
	}
	if !config.PassCheckRoot || config.ResetPassword {
		if os.Getegid() != 0 {
			log.Fatal("Please execute this program with sudo or as a root user for the best experience.\n" +
				"If you are sure you are root user, use the --passcheckroot parameter to skip the check.\n" +
				"If you don't want to run as root or you are a non-linux user, use --lite please.\n" +
				"For example:\n" +
				"$ v2raya --lite",
			)
		}
	}
	if config.ResetPassword {
		err := configure.ResetAccounts()
		if err != nil {
			log.Fatal("checkEnvironment: %v", err)
		}
		fmt.Println("It will work after you restart v2rayA")
		os.Exit(0)
	}
	_, v2rayAListeningPort, err := net.SplitHostPort(config.Address)
	if err != nil {
		log.Fatal("checkEnvironment: %v", err)
	}
	if occupied, sockets, err := ports.IsPortOccupied([]string{v2rayAListeningPort + ":tcp"}); occupied {
		if err != nil {
			log.Fatal("netstat:", err)
		}
		for _, socket := range sockets {
			process, err := socket.Process()
			if err == nil {
				log.Fatal("Port %v is occupied by %v/%v", v2rayAListeningPort, process.Name, process.PID)
			}
		}
	}
}

func checkTProxySupportability() {
	if conf.GetEnvironmentConfig().Lite {
		return
	}
	//检查tproxy是否可以启用
	if err := service2.CheckAndProbeTProxy(); err != nil {
		log.Info("Cannot load TPROXY module: %v", err)
	}
}

func migrate(jsonConfPath string) (err error) {
	log.Info("Migrating json to nutsdb...")
	defer func() {
		if err != nil {
			log.Warn("Migrating failed: %v", err)
		} else {
			log.Info("Migrating complete")
		}
	}()
	b, err := os.ReadFile(jsonConfPath)
	if err != nil {
		return
	}
	var cfg configure.Configure
	if err = jsoniter.Unmarshal(b, &cfg); err != nil {
		return
	}
	if err = configure.SetConfigure(&cfg); err != nil {
		return
	}
	return nil
}

func initDBValue() {
	log.Info("init DB")
	err := configure.SetConfigure(configure.New())
	if err != nil {
		log.Fatal("initDBValue: %v", err)
	}
}

func migrateServerFormat() {
	serverRaw := configure.GetServers()
	var serverRawV2 []*configure.ServerRawV2
	for _, raw := range serverRaw {
		if raw.VmessInfo.Protocol == "" {
			raw.VmessInfo.Protocol = "vmess"
		}
		obj, err := serverObj.NewFromLink(raw.VmessInfo.Protocol, raw.VmessInfo.ExportToURL())
		if err != nil {
			log.Warn("failed to migrate: %v", raw.VmessInfo.Ps)
			continue
		}
		serverRawV2 = append(serverRawV2, &configure.ServerRawV2{
			ServerObj: obj,
			Latency:   raw.Latency,
		})
	}
	if len(serverRawV2) > 0 {
		err := configure.AppendServers(serverRawV2)
		if err != nil {
			log.Warn("failed to migrate: %v", err)
		}
	}
	subscriptionsRaw := configure.GetSubscriptions()
	var subV2 []*configure.SubscriptionRawV2
	for _, raw := range subscriptionsRaw {
		var serversV2 []configure.ServerRawV2
		for _, sraw := range raw.Servers {
			if sraw.VmessInfo.Protocol == "" {
				sraw.VmessInfo.Protocol = "vmess"
			}
			obj, err := serverObj.NewFromLink(sraw.VmessInfo.Protocol, sraw.VmessInfo.ExportToURL())
			if err != nil {
				log.Warn("failed to migrate: %v", sraw.VmessInfo.Ps)
				continue
			}
			serversV2 = append(serversV2, configure.ServerRawV2{
				ServerObj: obj,
				Latency:   sraw.Latency,
			})
		}
		subRawV2 := configure.SubscriptionRawV2{
			Remarks: raw.Remarks,
			Address: raw.Address,
			Status:  raw.Status,
			Servers: serversV2,
			Info:    raw.Info,
		}
		subV2 = append(subV2, &subRawV2)
	}
	if len(subV2) > 0 {
		err := configure.AppendSubscriptions(subV2)
		if err != nil {
			log.Warn("failed to migrate: %v", err)
		}
	}
}

func initConfigure() {
	//等待网络连通
	v2ray.CheckAndStopTransparentProxy()
	for {
		addrs, err := resolv.LookupHost("apple.com")
		if err == nil && len(addrs) > 0 {
			break
		}
		log.Alert("waiting for network connected")
		time.Sleep(5 * time.Second)
	}
	log.Alert("network is connected")
	//初始化配置
	jsonIteratorExtra.RegisterFuzzyDecoders()

	//db
	confPath := conf.GetEnvironmentConfig().Config
	if _, err := os.Stat(confPath); os.IsNotExist(err) {
		_ = os.MkdirAll(path.Dir(confPath), os.ModeDir|0750)
	}
	if configure.IsConfigureNotExists() {
		// need to migrate?
		camp := []string{path.Join(path.Dir(confPath), "v2raya.json"), "/etc/v2ray/v2raya.json", "/etc/v2raya/v2raya.json"}
		var success bool
		for _, jsonConfPath := range camp {
			if _, err := os.Stat(jsonConfPath); err == nil {
				log.Info("migrate from %v", jsonConfPath)
				err = migrate(jsonConfPath)
				if err == nil {
					success = true
					break
				}
			}
		}
		if !success {
			initDBValue()
		}
	} else {
		// need to migrate server format from v1 to v2?
		if (len(configure.GetServers())+len(configure.GetSubscriptions())) > 0 &&
			(len(configure.GetServersV2())+len(configure.GetSubscriptionsV2())) == 0 {
			log.Info("migrating server format from v1 to v2...")
			migrateServerFormat()
		}
	}
	//检查config.json是否存在
	if _, err := os.Stat(asset.GetV2rayConfigPath()); err != nil {
		//不存在就建一个。多数情况发生于docker模式挂载volume时覆盖了/etc/v2ray
		t := v2ray.Template{}
		_ = v2ray.WriteV2rayConfig(t.ToConfigBytes())
	}

	//首先确定v2ray是否存在
	if _, err := where.GetV2rayBinPath(); err == nil {
		//检查geoip、geosite是否存在
		if !asset.IsGeoipExists() || !asset.IsGeositeExists() {
			dld := func(repo, filename, localname string) (err error) {
				log.Warn("installing " + filename)
				p := path.Join(asset.GetV2rayLocationAsset(), filename)
				resp, err := http.Get("https://api.github.com/repos/" + repo + "/tags")
				if err != nil {
					return
				}
				defer resp.Body.Close()
				b, err := io.ReadAll(resp.Body)
				if err != nil {
					return
				}
				tag := gjson.GetBytes(b, "0.name").String()
				u := fmt.Sprintf("https://cdn.jsdelivr.net/gh/%v@%v/%v", repo, tag, filename)
				err = gopeed.Down(&gopeed.Request{
					Method: "GET",
					URL:    u,
				}, p)
				if err != nil {
					return errors.New("download<" + p + ">: " + err.Error())
				}
				err = os.Chmod(p, os.FileMode(0755))
				if err != nil {
					return errors.New("chmod: " + err.Error())
				}
				os.Rename(p, path.Join(asset.GetV2rayLocationAsset(), localname))
				return
			}
			err := dld("v2rayA/dist-geoip", "geoip.dat", "geoip.dat")
			if err != nil {
				log.Warn("initConfigure: v2rayA/dist-geoip: %v", err)
			}
			err = dld("v2rayA/dist-domain-list-community", "dlc.dat", "geosite.dat")
			if err != nil {
				log.Warn("initConfigure: v2rayA/dist-domain-list-community: %v", err)
			}
		}
	}
}

func hello() {
	log.Alert("V2RayLocationAsset is %v", asset.GetV2rayLocationAsset())
	v2rayPath, _ := where.GetV2rayBinPath()
	log.Alert("V2Ray binary is %v", v2rayPath)
	wd, _ := os.Getwd()
	log.Alert("v2rayA working directory is %v", wd)
	log.Alert("v2rayA configuration directory is %v", conf.GetEnvironmentConfig().Config)
	log.Alert("Golang: %v", runtime.Version())
	log.Alert("OS: %v", runtime.GOOS)
	log.Alert("Arch: %v", runtime.GOARCH)
	log.Alert("Lite: %v", conf.GetEnvironmentConfig().Lite)
	log.Alert("Version: %v", conf.Version)
	log.Alert("Starting...")
}

func updateSubscriptions() {
	subs := configure.GetSubscriptionsV2()
	lenSubs := len(subs)
	control := make(chan struct{}, 2) //并发限制同时更新2个订阅
	wg := new(sync.WaitGroup)
	for i := 0; i < lenSubs; i++ {
		wg.Add(1)
		go func(i int) {
			control <- struct{}{}
			err := service.UpdateSubscription(i, false)
			if err != nil {
				log.Info("[AutoUpdate] Subscriptions: Failed to update subscription -- ID: %d，err: %v", i, err)
			} else {
				log.Info("[AutoUpdate] Subscriptions: Complete updating subscription -- ID: %d，Address: %s", i, subs[i].Address)
			}
			wg.Done()
			<-control
		}(i)
	}
	wg.Wait()
}

func initUpdatingTicker() {
	conf.TickerUpdateGFWList = time.NewTicker(24 * time.Hour * 365 * 100)
	conf.TickerUpdateSubscription = time.NewTicker(24 * time.Hour * 365 * 100)
	go func() {
		for range conf.TickerUpdateGFWList.C {
			_, err := gfwlist.CheckAndUpdateGFWList()
			if err != nil {
				log.Info("[AutoUpdate] GFWList: %v", err)
			}
		}
	}()
	go func() {
		for range conf.TickerUpdateSubscription.C {
			updateSubscriptions()
		}
	}()
}

func checkUpdate() {
	setting := service.GetSetting()

	//初始化ticker
	initUpdatingTicker()

	//检查PAC文件更新
	if setting.GFWListAutoUpdateMode == configure.AutoUpdate ||
		setting.GFWListAutoUpdateMode == configure.AutoUpdateAtIntervals ||
		setting.Transparent == configure.TransparentGfwlist {
		if setting.GFWListAutoUpdateMode == configure.AutoUpdateAtIntervals {
			conf.TickerUpdateGFWList.Reset(time.Duration(setting.GFWListAutoUpdateIntervalHour) * time.Hour)
		}
		switch setting.RulePortMode {
		case configure.GfwlistMode:
			go func() {
				/* 更新LoyalsoldierSite.dat */
				localGFWListVersion, err := gfwlist.CheckAndUpdateGFWList()
				if err != nil {
					log.Warn("Failed to update PAC file: %v", err.Error())
					return
				}
				log.Info("Complete updating PAC file. Localtime: %v", localGFWListVersion)
			}()
		case configure.CustomMode:
			// obsolete
		}
	}

	//检查订阅更新
	if setting.SubscriptionAutoUpdateMode == configure.AutoUpdate ||
		setting.SubscriptionAutoUpdateMode == configure.AutoUpdateAtIntervals {

		if setting.SubscriptionAutoUpdateMode == configure.AutoUpdateAtIntervals {
			conf.TickerUpdateSubscription.Reset(time.Duration(setting.SubscriptionAutoUpdateIntervalHour) * time.Hour)
		}
		go updateSubscriptions()
	}
	// 检查服务端更新
	go func() {
		f := func() {
			if foundNew, remote, err := service.CheckUpdate(); err == nil {
				conf.FoundNew = foundNew
				conf.RemoteVersion = remote
			}
		}
		f()
		c := time.Tick(7 * 24 * time.Hour)
		for range c {
			f()
		}
	}()
}

func run() (err error) {
	//判别需要启动v2ray吗
	if configure.GetRunning() {
		err := v2ray.UpdateV2RayConfig()
		if err != nil {
			log.Error("failed to start v2ray-core: %v", err)
		}
	}
	//w := configure.GetConnectedServers()
	//log.Println(err, ", which:", w)
	//_ = configure.ClearConnected()
	errch := make(chan error)
	//启动服务端
	go func() {
		errch <- router.Run()
	}()
	//监听信号，处理透明代理的关闭
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGILL)
		<-sigs
		errch <- nil
	}()
	if err = <-errch; err != nil {
		log.Fatal("run: %v", err)
	}
	fmt.Println("Quitting...")
	v2ray.CheckAndStopTransparentProxy()
	v2ray.ProcessManager.Stop(false)
	_ = db.DB().Close()
	return nil
}
