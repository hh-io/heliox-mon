package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/hh/heliox-mon/internal/api"
	"github.com/hh/heliox-mon/internal/collector"
	"github.com/hh/heliox-mon/internal/config"
	"github.com/hh/heliox-mon/internal/notifier"
	"github.com/hh/heliox-mon/internal/storage"
)

func main() {
	// 加载配置
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 初始化数据库
	db, err := storage.NewDB(cfg.DataDir)
	if err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	defer db.Close()

	// 初始化通知器
	ntf := notifier.New(cfg, db)

	// 初始化采集器
	col := collector.New(cfg, db, ntf)
	col.Start()
	defer col.Stop()

	// 启动 HTTP 服务（传入采集器作为实时数据源）
	server := api.NewServer(cfg, db, col)
	go func() {
		if err := server.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务启动失败: %v", err)
		}
	}()

	// 优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("正在关闭服务...")
	server.Stop()
}
