// vaultctl 是 macOS Keychain 凭据管理 CLI（M2-G3）。
//
// 用法：
//
//	vaultctl store <service> <account>   # 从 stdin 读入密钥并写入系统 Keychain
//	vaultctl get <service> <account>     # 打印密钥引用（不打印原始密钥）
//	vaultctl delete <service> <account>
//	vaultctl redact <text...>            # 脱敏文本
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"midroute/internal/credentials"
)

func main() {
	flag.Parse()
	if len(flag.Args()) < 1 {
		usage()
		os.Exit(2)
	}
	cmd := flag.Arg(0)
	switch cmd {
	case "store":
		cmdStore(flag.Args()[1:], os.Stdin)
	case "get":
		cmdGet(flag.Args()[1:])
	case "delete":
		cmdDelete(flag.Args()[1:])
	case "redact":
		cmdRedact(flag.Args()[1:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: vaultctl <store|get|delete|redact> ...")
}

func kv() credentials.Vault {
	kv, err := credentials.NewKeychainVault("midroute")
	if err != nil {
		fmt.Fprintln(os.Stderr, "vault:", err)
		os.Exit(1)
	}
	return kv
}

func cmdStore(args []string, in io.Reader) {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: vaultctl store <service> <account>  (secret via stdin)")
		os.Exit(2)
	}
	secret, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "read stdin:", err)
		os.Exit(1)
	}
	secret = strings.TrimRight(secret, "\r\n")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "empty secret")
		os.Exit(1)
	}
	ref, err := kv().Store(args[0], args[1], []byte(secret))
	if err != nil {
		fmt.Fprintln(os.Stderr, "store:", err)
		os.Exit(1)
	}
	fmt.Printf("stored ref: service=%s account=%s fingerprint=%s\n", ref.Service, ref.Account, ref.Fingerprint)
}

func cmdGet(args []string) {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: vaultctl get <service> <account>")
		os.Exit(2)
	}
	s, err := kv().Get(credentials.SecretRef{Service: args[0], Account: args[1]})
	if err != nil {
		fmt.Fprintln(os.Stderr, "get:", err)
		os.Exit(1)
	}
	defer s.Zero()
	// 只打印引用信息，不打印原始密钥
	fmt.Printf("found: fingerprint=%s len=%d\n", credentials.ShortFingerprint(s.Value), len(s.Value))
}

func cmdDelete(args []string) {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: vaultctl delete <service> <account>")
		os.Exit(2)
	}
	if err := kv().Delete(credentials.SecretRef{Service: args[0], Account: args[1]}); err != nil {
		fmt.Fprintln(os.Stderr, "delete:", err)
		os.Exit(1)
	}
	fmt.Println("deleted")
}

func cmdRedact(args []string) {
	r := credentials.NewRedactor()
	fmt.Fprintln(os.Stdout, r.Redact(strings.Join(args, " ")))
}
