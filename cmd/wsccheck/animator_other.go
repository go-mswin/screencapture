// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !windows

package main

import "errors"

// Off Windows there is no window to open, so -animate reports why rather than
// failing to build: wsccheck itself must cross-compile like the package it
// verifies.
type animator struct {
	hwnd uintptr
	w, h int
}

func startAnimator(w, h int) (*animator, error) {
	return nil, errors.New("the animation window needs Windows")
}

func (a *animator) stop() {}
