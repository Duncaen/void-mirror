package main

import (
	"flag"
	"log"

	"github.com/void-linux/void-mirror/config"
)

var (
	conffile = flag.String("conffile", "config.hcl", "configuration file path")
)

func main() {
	flag.Parse()
	var conf config.Config
	err := conf.Load(*conffile)
	if err != nil {
		log.Fatal(err)
	}
}
