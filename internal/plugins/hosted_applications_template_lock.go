package plugins

import (
	"bytes"
	"embed"
	"fmt"
)

//go:embed hosted_app_templates/next/package-lock.json
var hostedAppTemplateFiles embed.FS

func hostedAppNextPackageLock(id string) []byte {
	lock, err := hostedAppTemplateFiles.ReadFile("hosted_app_templates/next/package-lock.json")
	if err != nil {
		panic(fmt.Sprintf("read embedded Next.js package lock: %v", err))
	}
	return bytes.ReplaceAll(lock, []byte("folio-next-template"), []byte(id))
}
