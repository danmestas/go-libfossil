//go:build !js

package repo

import "github.com/danmestas/go-libfossil/simio"

func checkExists(env *simio.Env, path string) error {
	if _, err := env.Storage.Stat(path); err != nil {
		return err
	}
	return nil
}
