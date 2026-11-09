package main

import (
	"crypto/rand"
	"encoding/base64"

	"meoo2api/internal/model"
)

func randomPassword() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashPassword(pw string) string { return model.ShaHex(pw) }
