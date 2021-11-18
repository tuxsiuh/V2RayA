package db

import (
	"github.com/v2rayA/v2rayA/conf"
	"github.com/xujiajun/nutsdb"
	"log"
	"os"
	"sync"
)

var once sync.Once
var db *nutsdb.DB

func initDB() {
	confPath := conf.GetEnvironmentConfig().Config
	var err error
	opt := nutsdb.DefaultOptions
	opt.Dir = confPath
	db, err = nutsdb.Open(opt)
	// for privacy
	defer os.Chmod(confPath, os.ModeDir|0750)
	if err != nil {
		log.Fatal(err)
	}
}

func DB() *nutsdb.DB {
	once.Do(initDB)
	return db
}
