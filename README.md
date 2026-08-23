<h1 align="center">nanogo</h1>

<p align="center">
  <strong>A tiny Go compiler.</strong>
</p>

<p align="center">
  <a href="https://pkg.go.dev/golang.design/x/nanogo"><img src="https://pkg.go.dev/badge/golang.design/x/nanogo.svg" alt="Go Reference"></a>
  <a href="https://github.com/golang-design/nanogo/actions/workflows/ci.yml"><img src="https://github.com/golang-design/nanogo/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-BSD--3--Clause-blue.svg" alt="License: BSD-3-Clause"></a>
  <img src="https://img.shields.io/badge/go-1.27+-00ADD8.svg" alt="Go 1.27+">
  <img src="https://img.shields.io/badge/status-early-orange.svg" alt="Status: early">
</p>

---

nanogo compiles Go source code. It is small on purpose: the goal is a
compiler you can read end to end, not a replacement for the toolchain.

A second goal shapes the design. GPU kernels in
[accel](https://github.com/golang-design/accel) are written in a subset of
Go and compiled ahead of time. nanogo is built with that class of target in
mind, so the same front end can serve both a general Go program and a
compute kernel.

> [!IMPORTANT]
> **Nothing works yet.** The repository holds the module identity, the
> license and CI. The architecture is not decided. Do not depend on this.

## Install

```sh
go get golang.design/x/nanogo
```

## License

BSD-3-Clause &copy; 2026 The [golang.design](https://golang.design) Initiative Authors
