package main

import (
	"crypto/rand"
	"encoding/base64"

	"meoo2api/internal/model"
)

func randomPassword() string {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashPassword(pw string) string { return model.HashPassword(pw) }
