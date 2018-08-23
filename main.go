package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/Gurpartap/logrus-stack"
	"github.com/pyama86/pftp/example/restapi"
	"github.com/pyama86/pftp/example/restapi/testserver"
	"github.com/pyama86/pftp/pftp"
	"github.com/sirupsen/logrus"
)

var ftpServer *pftp.FtpServer

var confFile = "./config.toml"

func init() {
	logrus.SetLevel(logrus.DebugLevel)
	stackLevels := []logrus.Level{logrus.PanicLevel, logrus.FatalLevel}
	logrus.AddHook(logrus_stack.NewHook(stackLevels, stackLevels))
}

func main() {
	go func() {
		restServer.NewRestServer()
	}()

	ftpServer, err := pftp.NewFtpServer(confFile)
	if err != nil {
		logrus.Fatal(err)
	}
	go signalHandler()

	ftpServer.Use("user", User)
	if err := ftpServer.ListenAndServe(); err != nil {
		logrus.Fatal(err)
	}
}

func signalHandler() {
	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGTERM)
	for {
		switch <-ch {
		case syscall.SIGTERM:
			ftpServer.Stop()
			break
		}
	}
}

func User(c *pftp.Context, param string) error {
	host, err := restapi.GetDomainFromWebAPI(confFile, param)
	if err == nil {
		c.RemoteAddr = *host
	}

	return nil
}
