package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	authCookieName      = "eg_session"
	authSessionDuration = 7 * 24 * time.Hour
	settingPasswordHash = "auth_password_hash"
	settingAuthSecret   = "auth_secret"
)

// hashPassword 返回 "saltHex:hashHex"。
func hashPassword(password, saltHex string) string {
	if saltHex == "" {
		buf := make([]byte, 16)
		_, _ = rand.Read(buf)
		saltHex = hex.EncodeToString(buf)
	}
	sum := sha256.Sum256([]byte(saltHex + ":" + password))
	return saltHex + ":" + hex.EncodeToString(sum[:])
}

func verifyPassword(stored, password string) bool {
	parts := strings.SplitN(stored, ":", 2)
	if len(parts) != 2 {
		return false
	}
	expected := hashPassword(password, parts[0])
	return subtle.ConstantTimeCompare([]byte(stored), []byte(expected)) == 1
}

func (a *WebApp) getSetting(key string) (string, error) {
	var value string
	err := a.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err != nil {
		return "", err
	}
	return value, nil
}

func (a *WebApp) putSetting(key, value string) error {
	_, err := a.db.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, time.Now().Format(time.RFC3339Nano),
	)
	return err
}

// initAuth 按优先级确定管理密码:显式传入 > 已存哈希 > 自动生成。
// 返回是否需要在启动日志里提示用户去 auth_password.txt 看密码。
func (a *WebApp) initAuth(explicitPassword string) error {
	if explicitPassword != "" {
		if err := a.putSetting(settingPasswordHash, hashPassword(explicitPassword, "")); err != nil {
			return err
		}
		fmt.Println("管理密码已通过 -auth-password 设置")
	} else {
		if _, err := a.getSetting(settingPasswordHash); err != nil {
			// 还没有任何密码,生成一个随机密码。
			buf := make([]byte, 9)
			if _, err := rand.Read(buf); err != nil {
				return err
			}
			generated := base64.RawURLEncoding.EncodeToString(buf)
			if err := a.putSetting(settingPasswordHash, hashPassword(generated, "")); err != nil {
				return err
			}
			pwFile := filepath.Join(a.dataDir, "auth_password.txt")
			if err := os.WriteFile(pwFile, []byte(generated+"\n"), 0o600); err != nil {
				return err
			}
			fmt.Printf("已生成初始管理密码并写入: %s\n", pwFile)
			fmt.Printf("初始管理密码: %s\n", generated)
		}
	}

	if _, err := a.getSetting(settingAuthSecret); err != nil {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		if err := a.putSetting(settingAuthSecret, hex.EncodeToString(buf)); err != nil {
			return err
		}
	}
	return nil
}

func (a *WebApp) authSecret() ([]byte, error) {
	s, err := a.getSetting(settingAuthSecret)
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(s)
}

// signSession 生成 "expiryUnix.signatureHex"。
func signSession(secret []byte, expiry time.Time) string {
	ts := strconv.FormatInt(expiry.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts))
	return ts + "." + hex.EncodeToString(mac.Sum(nil))
}

func verifySession(secret []byte, token string) bool {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > ts {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0]))
	expected := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(expected), []byte(parts[1])) == 1
}

func (a *WebApp) validSession(r *http.Request) bool {
	cookie, err := r.Cookie(authCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	secret, err := a.authSecret()
	if err != nil {
		return false
	}
	return verifySession(secret, cookie.Value)
}

func (a *WebApp) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.validSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(loginPageHTML))
}

func (a *WebApp) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	stored, err := a.getSetting(settingPasswordHash)
	if err != nil || !verifyPassword(stored, req.Password) {
		http.Error(w, "密码错误", http.StatusUnauthorized)
		return
	}
	secret, err := a.authSecret()
	if err != nil {
		http.Error(w, "服务异常", http.StatusInternalServerError)
		return
	}
	expiry := time.Now().Add(authSessionDuration)
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    signSession(secret, expiry),
		Path:     "/",
		Expires:  expiry,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *WebApp) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// authMiddleware 保护全部路由:登录页/登录接口/健康检查放行,
// 其余页面 302 到 /login,API 返回 401。
func (a *WebApp) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/login" || p == "/api/auth/login" || p == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		if a.validSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(p, "/api/") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "未登录或登录已过期"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})
}

const loginPageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8" />
<meta name="viewport" content="width=device-width, initial-scale=1.0" />
<title>登录 - Email Get</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: -apple-system, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
         background: #f0f2f5; display: flex; align-items: center; justify-content: center; min-height: 100vh; }
  .card { background: #fff; padding: 40px 36px; border-radius: 10px; width: 340px;
          box-shadow: 0 4px 20px rgba(0,0,0,.08); }
  h1 { font-size: 20px; margin-bottom: 24px; text-align: center; color: #303133; }
  input { width: 100%; padding: 11px 12px; border: 1px solid #dcdfe6; border-radius: 6px;
          font-size: 14px; outline: none; }
  input:focus { border-color: #409eff; }
  button { width: 100%; margin-top: 16px; padding: 11px; border: none; border-radius: 6px;
           background: #409eff; color: #fff; font-size: 15px; cursor: pointer; }
  button:hover { background: #66b1ff; }
  button:disabled { background: #a0cfff; cursor: not-allowed; }
  .err { color: #f56c6c; font-size: 13px; margin-top: 12px; min-height: 18px; text-align: center; }
</style>
</head>
<body>
  <div class="card">
    <h1>Email Get 登录</h1>
    <form id="f">
      <input type="password" id="pw" placeholder="请输入管理密码" autocomplete="current-password" autofocus />
      <button type="submit" id="btn">登 录</button>
      <div class="err" id="err"></div>
    </form>
  </div>
<script>
document.getElementById('f').addEventListener('submit', async function (e) {
  e.preventDefault();
  var btn = document.getElementById('btn');
  var err = document.getElementById('err');
  btn.disabled = true; err.textContent = '';
  try {
    var resp = await fetch('/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: document.getElementById('pw').value })
    });
    if (resp.ok) { location.href = '/'; return; }
    err.textContent = (await resp.text()) || '登录失败';
  } catch (ex) {
    err.textContent = '网络错误,请重试';
  } finally {
    btn.disabled = false;
  }
});
</script>
</body>
</html>
`
