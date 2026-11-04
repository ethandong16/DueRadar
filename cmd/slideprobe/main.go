package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ethandong16/due-radar/internal/platform"
)

func main() {
	phone := os.Getenv("PROBE_PHONE")
	password := os.Getenv("PROBE_PASSWORD")
	validate := os.Getenv("PROBE_VALIDATE")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	started := time.Now()
	_, err := platform.SlideLogin(ctx, phone, password, validate, nil)
	fmt.Printf("elapsed=%s err=%v\n", time.Since(started).Round(time.Millisecond), err)
}
