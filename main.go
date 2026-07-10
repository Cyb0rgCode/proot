// proot — pm2's dashboard with tmux's interactivity.
//
//	proot serve [--listen 127.0.0.1:8689]   start the agent + web UI
//	proot run --id <id>                     internal: app wrapper (run by tmux)
package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"os"

	"proot/internal/runner"
	"proot/internal/server"
)

//go:embed web
var webEmbed embed.FS

const defaultListen = "127.0.0.1:8689" // 8689 = t-m-u-x on a phone keypad

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		listen := fs.String("listen", defaultListen,
			"address to listen on (use 0.0.0.0:8689 to expose on the LAN)")
		fs.Parse(os.Args[2:])
		if err := serve(*listen); err != nil {
			fmt.Fprintln(os.Stderr, "proot:", err)
			os.Exit(1)
		}
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		id := fs.String("id", "", "app id (from apps.json)")
		fs.Parse(os.Args[2:])
		os.Exit(runner.Main(*id))
	case "version", "--version", "-v":
		fmt.Println("proot 0.1.0")
	default:
		usage()
		os.Exit(2)
	}
}

func serve(listen string) error {
	webFS, err := fs.Sub(webEmbed, "web")
	if err != nil {
		return err
	}
	srv, err := server.New(webFS)
	if err != nil {
		return err
	}
	return srv.ListenAndServe(listen)
}

func usage() {
	fmt.Fprintln(os.Stderr, `proot — a web dashboard for everything running in tmux

usage:
  proot serve [--listen ADDR]   start the agent and web UI (default `+defaultListen+`)
  proot version                 print version`)
}
