package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/model"
	"dsgo/internal/state"
)

func credentials(cfg state.Config) (string, string, error) {
	if cfg.APIKey != "" {
		return cfg.APIKey, "", nil
	}
	if len(cfg.Account) == 0 {
		return "", "", nil
	}
	var grant model.AccountGrant
	if err := json.Unmarshal(cfg.Account, &grant); err != nil {
		return "", "", errors.New("invalid saved account grant")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return "", "", err
	}
	if u.Scheme != "https" || u.Host != "api.deepseek.com" || grant.Issuer != model.DefaultPlatformOrigin {
		return "", "", errors.New("saved account credentials may only be sent to official DeepSeek endpoints")
	}
	return "", grant.Token, nil
}

func (a *App) auth(args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	var grant model.AccountGrant
	if len(a.cfg.Account) > 0 {
		if err := json.Unmarshal(a.cfg.Account, &grant); err != nil {
			return err
		}
	}
	_, offset := time.Now().Zone()
	client, err := model.NewAccount(model.AccountOptions{DeviceID: grant.DeviceID, ClientVersion: Version, Locale: a.cfg.Language, TimezoneOffsetSeconds: offset})
	if err != nil {
		return err
	}
	ctx, cancel := signalContext(context.Background())
	defer cancel()
	switch args[0] {
	case "login":
		if a.flags.json {
			return errors.New("interactive login requires browser authorization; use an API key in automation")
		}
		_, err = client.Login(ctx, func(u string) error {
			a.line("Authorize this device in your browser:\n%s\n", u)
			if a.flags.json {
				return errors.New("interactive login requires browser authorization; use an API key in automation")
			}
			var cmd *exec.Cmd
			switch runtime.GOOS {
			case "darwin":
				cmd = exec.CommandContext(ctx, "open", u)
			case "linux":
				cmd = exec.CommandContext(ctx, "xdg-open", u)
			case "windows":
				cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", u)
			default:
				return errors.New("cannot open an authorization browser on this platform")
			}
			if x := cmd.Run(); x != nil {
				a.line("Browser launch failed; open the authorization URL manually.\n")
			}
			return nil
		}, func(g model.AccountGrant) error {
			a.cfg.Account = core.JSON(g)
			return state.SaveConfig(a.flags.home, a.cfg)
		})
		if err == nil {
			a.line("Signed in.\n")
		}
		return err
	case "logout":
		a.cfg.Account = nil
		if err = state.SaveConfig(a.flags.home, a.cfg); err != nil {
			return err
		}
		if grant.Token != "" {
			if err = client.Logout(ctx, grant); err != nil {
				a.line("Signed out locally. Remote token revocation failed: %s\n", clean(err.Error()))
				return nil
			}
		}
		a.line("Signed out.\n")
		return nil
	case "status":
		a.print(map[string]any{"account_authenticated": grant.Token != "", "api_key_configured": a.cfg.WithEnvironment().APIKey != ""})
		return nil
	case "profile":
		if grant.Token == "" {
			return errors.New("not signed in")
		}
		profile, err := client.Profile(ctx, grant)
		if err != nil {
			return err
		}
		a.print(profile)
		return nil
	default:
		return fmt.Errorf("unknown auth action %q; use login, logout, status, profile", args[0])
	}
}
