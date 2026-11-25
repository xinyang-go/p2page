package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"p2page/internal/client"
	"p2page/internal/server"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: p2page server|client -config path.json")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	path := fs.String("config", "", "JSON configuration file")
	_ = fs.Parse(os.Args[2:])
	if *path == "" {
		log.Fatal("-config is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "server":
		err = server.Run(ctx, *path)
	case "client":
		err = client.Run(ctx, *path)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}
