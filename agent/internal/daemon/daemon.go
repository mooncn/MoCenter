// Package daemon 提供 Agent 进程守护与崩溃自启能力。
// 当 Agent 主循环因异常退出时，自动重新拉起，确保不被异常进程或崩溃杀死。
package daemon

import (
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Supervise 以守护方式运行 fn。fn 为 Agent 主循环函数。
// 当 fn 返回（异常退出）时，在短暂延迟后重新调用，实现崩溃自启。
// 仅当收到 SIGTERM/SIGINT 信号时才真正退出。
func Supervise(fn func()) {
	log.Printf("[daemon] 守护进程启动，监视 Agent 主循环")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	var wg sync.WaitGroup

	// 崩溃自启循环
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("[daemon] Agent 主循环 panic: %v，准备重启", r)
						}
					}()
					fn()
				}()
				// fn 正常/异常返回，等待 3 秒后重启（避免高频重启）
				select {
				case <-stop:
					return
				case <-time.After(3 * time.Second):
					log.Printf("[daemon] Agent 进程退出，3 秒后自动重启")
				}
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		sig := <-stop
		log.Printf("[daemon] 收到信号 %v，退出守护进程", sig)
		os.Exit(0)
	}()
	wg.Wait()
}
