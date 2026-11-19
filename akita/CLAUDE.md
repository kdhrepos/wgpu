# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

Architecture-agnostic discrete-event simulator framework. Everything else in this workspace (`mem`, `noc`, `mgpusim`, ...) builds on these primitives. Do not import anything outside the standard library here — `akita` is the dependency *root*. Detailed docs live in `doc/index.md` (event/component/connection/hook systems).

## Core abstractions

- `Component` (`component.go`) — base unit. Owns `Port`s, registers `Hook`s, lives on an `Engine`. Concrete components elsewhere embed `*ComponentBase`.
- `Port` (`port.go`) — bidirectional message endpoint on a `Component`. `Send`/`Recv` of `Msg` (`msg.go`).
- `Connection` (`connection.go`, `directconnection.go`) — routes `Msg`s between `Port`s, may add latency. `DirectConnection` is zero-latency.
- `Engine` (`engine.go`, `serialengine.go`, `parallelengine.go`) — schedules `Event`s. The runner picks one via `-parallel`. The `EventQueue` (`eventqueue.go`) is a min-heap keyed by `Time`.
- `Event` (`event.go`) — `Handle()`d by the component at its scheduled `VTimeInSec`.
- `Hook` (`hook.go`, `loghook.go`, `portmsglogger.go`) — observer that fires on component/port lifecycle and message events. Tracing in `util/tracing` is built on top of this.
- `Ticker` (`ticker.go`) — drives recurring per-cycle work for components that want a tick loop.
- `freq.go` — `Freq` type with `Period()`, `ThisTick()`, `NextTick()` helpers; clock domains live here.
- `idgenerator.go` — global monotonic ID source for messages/events; use `akita.GetIDGenerator()` rather than rolling your own.

## Conventions

- `doc/coding_guideline.md` is enforced:
  - 80-char lines; gofmt + goimports on save.
  - **No comment paragraphs explaining code** — extract a named function instead. This is a hard rule in this repo and applies to everything that depends on `akita`.
  - Functions ≤10 lines, ≤6 args (constructors included). When wrapping, indent args on their own lines.
- Builder pattern everywhere: `MakeXxxBuilder().WithFoo(...).Build()`. Constructors that take many args are split into chained `With*` setters.
- No singleton state inside `akita` (process-wide singletons live in `util/global`). Engines are passed in explicitly.

## Linting / tests

`.golangci.yml` is present at the repo root and configures golangci-lint with strict settings. Run lint per-module:
```bash
golangci-lint run ./...
go test ./...
```

## Modifying this module

Because every other module replaces `gitlab.com/akita/akita => ../akita`, any change to a public API here ripples through the entire workspace. Before renaming or changing a signature on `Component`, `Port`, `Engine`, `Msg`, `Hook`, or `Event`, grep across sibling modules — there will be many call sites.
