package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/erning/cida-cli/internal/config"
	"github.com/erning/cida-cli/internal/llm"
)

// Override when building: go build -ldflags '-X main.version=v1.0.0'.
var version = "dev"

const usage = `Usage: cida [OPTIONS] [TEXT...]

Translate or improve text with your configured language model.
Without TEXT, read all of standard input. Options must precede TEXT;
use -- before text that starts with a dash.

Options:
  -a, --action ACTION  translate (default), improve, or a custom action name
      --config PATH    configuration file (default: ~/.config/cida/config.json)
      --print-prompt   print the assembled system prompt without source text
  -h, --help           show this help
      --version        show the version

Actions:
  translate  Detect the source language: my_language -> foreign_language,
             any other language -> my_language.
  improve    Improve clarity and grammar, preserving language and meaning.
  Custom actions are defined in the configuration's actions array.

Examples:
  cida "你好"
  cida --action translate "Hello"
  cida -a improve "This are a example."
  cida --print-prompt -a translate
  cat article.txt | cida
  cida -- "--help is a command-line option"
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	info, err := os.Stdin.Stat()
	interactive := err == nil && info.Mode()&os.ModeCharDevice != 0
	code := run(ctx, os.Args[1:], os.Stdin, interactive, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, interactive bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cida", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var actionName, path string
	var help, showVersion, printPrompt bool
	fs.StringVar(&actionName, "action", config.Translate, "action to run")
	fs.StringVar(&actionName, "a", config.Translate, "action to run")
	fs.StringVar(&path, "config", "", "configuration path")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	fs.BoolVar(&showVersion, "version", false, "show version")
	fs.BoolVar(&printPrompt, "print-prompt", false, "print the system prompt without sending a request")
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if help {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if showVersion {
		fmt.Fprintln(stdout, "cida", version)
		return 0
	}

	text := strings.Join(fs.Args(), " ")
	if !printPrompt && fs.NArg() == 0 {
		if interactive {
			fmt.Fprintln(stderr, "cida: provide text as arguments or through standard input; see cida --help")
			return 2
		}
		data, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "cida: read input:", err)
			return 1
		}
		text = string(data)
	}
	if !printPrompt && strings.TrimSpace(text) == "" {
		fmt.Fprintln(stderr, "cida: the input text is empty")
		return 2
	}
	if path == "" {
		var err error
		path, err = config.Path()
		if err != nil {
			fmt.Fprintln(stderr, "cida:", err)
			return 2
		}
	}
	loadConfig := config.Load
	if printPrompt {
		loadConfig = config.LoadForPrompt
	}
	cfg, err := loadConfig(path)
	if err != nil {
		fmt.Fprintf(stderr, "cida: configuration %s: %v\n", path, err)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stderr, "Create it using config.example.json.")
		}
		return 2
	}
	action, ok := cfg.FindAction(actionName)
	if !ok {
		var names []string
		for _, a := range cfg.ActionList() {
			names = append(names, a.ID)
		}
		fmt.Fprintf(stderr, "cida: unknown action %q; available: %s\n", actionName, strings.Join(names, ", "))
		return 2
	}

	if printPrompt {
		_, err := fmt.Fprintln(stdout, llm.BuildPrompt(cfg, action, "").System)
		if err != nil {
			fmt.Fprintln(stderr, "cida: write output:", err)
			return 1
		}
		return 0
	}

	// Stop the request if the output consumer fails or closes a pipe.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	last := byte('\n')
	var writeErr error
	err = llm.Run(ctx, cfg, action, text, func(s string) {
		if s == "" || writeErr != nil {
			return
		}
		_, writeErr = io.WriteString(stdout, s)
		last = s[len(s)-1]
		if writeErr != nil {
			cancel()
		}
	})
	if writeErr == nil && last != '\n' {
		_, writeErr = io.WriteString(stdout, "\n")
	}
	if writeErr != nil {
		fmt.Fprintln(stderr, "cida: write output:", writeErr)
		return 1
	}
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return 130
	}
	if err != nil {
		message := (llm.Redactor{Secret: cfg.APIKey}).Redact(fmt.Sprintf("cida: %s: %v\n", llm.Reason(err), err))
		fmt.Fprint(stderr, message)
		return 1
	}
	return 0
}
