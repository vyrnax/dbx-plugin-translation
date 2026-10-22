package secret

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"hash"
)

// errHKDFLength 表示请求的输出长度非法（<=0 或超过 255*HashLen）。
var errHKDFLength = errors.New("HKDF 输出长度非法")

// 本文件用标准库实现 HKDF-SHA256（RFC 5869），刻意【不】引入 golang.org/x/crypto/hkdf。
//
// 为什么不直接用 x/crypto：
//
//	x/crypto 最新版要求 go 1.26.0，会把 go.mod 的 go 指令顶高，
//	进而抬高整个项目的工具链下限。而 HKDF 本身只有 extract + expand 两步、
//	约 30 行代码，用标准库的 crypto/hmac + crypto/sha256 就能完整实现。
//	为一个 30 行的函数引入一个会绑架工具链版本的依赖，不划算。
//
// 实现严格遵循 RFC 5869：
//
//	Extract(salt, IKM) -> PRK          （第 2.2 节）
//	Expand(PRK, info, L) -> OKM        （第 2.3 节）
//
// 我们只需要 32 字节输出（AES-256 密钥），恰好等于 SHA-256 的 HashLen，
// 因此 Expand 只跑一轮（T(1)），不需要循环。

// hkdfExtract 实现 RFC 5869 第 2.2 节的 Extract 步骤。
//
//	PRK = HMAC-Hash(salt, IKM)
//
// salt 为空时 RFC 规定使用 HashLen 个零字节。我们的 salt 是固定常量，
// 不会为空，但仍按规范处理以保证正确性。
func hkdfExtract(salt, ikm []byte) []byte {
	if len(salt) == 0 {
		salt = make([]byte, sha256.Size)
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	return mac.Sum(nil)
}

// hkdfExpand 实现 RFC 5869 第 2.3 节的 Expand 步骤。
//
//	T(0) = 空串
//	T(i) = HMAC-Hash(PRK, T(i-1) | info | i)
//	OKM  = 前 L 字节 of T(1) | T(2) | ...
//
// 其中 i 是单字节计数器（1..255），因此 L 最大为 255*HashLen。
// 我们只要 32 字节，一轮就够。
func hkdfExpand(prk, info []byte, length int) ([]byte, error) {
	hashLen := sha256.Size
	if length <= 0 {
		return nil, errHKDFLength
	}
	// RFC 5869 第 2.3 节：L 不能超过 255*HashLen
	if length > 255*hashLen {
		return nil, errHKDFLength
	}

	var (
		mac  hash.Hash = hmac.New(sha256.New, prk)
		out  []byte
		prev []byte
	)
	for counter := 1; len(out) < length; counter++ {
		mac.Reset()
		mac.Write(prev)
		mac.Write(info)
		mac.Write([]byte{byte(counter)})
		prev = mac.Sum(nil)
		out = append(out, prev...)
	}
	return out[:length], nil
}

// hkdfSHA256 是 Extract + Expand 的组合，一步派生出指定长度的密钥。
func hkdfSHA256(secret, salt, info []byte, length int) ([]byte, error) {
	prk := hkdfExtract(salt, secret)
	return hkdfExpand(prk, info, length)
}
