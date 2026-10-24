package main

/*
To run this code and generate a new key set, use the following command:
go run install/generate-ed25519-keys.go

It prints raw base64 Ed25519 (signing) and X25519 (encryption) key pairs in the
simplekeymanager format; signingPrivateKey is the 32-byte Ed25519 seed.
*/

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
)

func main() {
	// Generate a new Ed25519 key pair using rand.Reader as default
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		log.Fatal("Failed to generate key pair:", err)
	}

	// The private key contains both seed and public key (64 bytes total)
	// We need to extract just the seed (first 32 bytes)
	seed := privateKey[:ed25519.SeedSize]

	encrPrivateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		log.Fatal("Failed to generate encryption key pair:", err)
	}

	fmt.Println("=== Ed25519 Key Pair ===")
	fmt.Printf("signingPrivateKey: %s\n", base64.StdEncoding.EncodeToString(seed))
	fmt.Printf("signingPublicKey: %s\n", base64.StdEncoding.EncodeToString(publicKey))
	fmt.Println("=== X25519 Key Pair ===")
	fmt.Printf("encrPrivateKey: %s\n", base64.StdEncoding.EncodeToString(encrPrivateKey.Bytes()))
	fmt.Printf("encrPublicKey: %s\n", base64.StdEncoding.EncodeToString(encrPrivateKey.PublicKey().Bytes()))
}
