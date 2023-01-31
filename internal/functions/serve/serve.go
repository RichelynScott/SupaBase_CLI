package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/joho/godotenv"
	"github.com/spf13/afero"
	"github.com/supabase/cli/internal/utils"
)

const (
	relayFuncDir              = "/home/deno/functions"
	customDockerImportMapPath = "/home/deno/import_map.json"
)

func ParseEnvFile(envFilePath string) ([]string, error) {
	env := []string{}
	if len(envFilePath) == 0 {
		return env, nil
	}
	envMap, err := godotenv.Read(envFilePath)
	if err != nil {
		return env, err
	}
	for name, value := range envMap {
		if strings.HasPrefix(name, "SUPABASE_") {
			return env, errors.New("Invalid env name: " + name + ". Env names cannot start with SUPABASE_.")
		}
		env = append(env, name+"="+value)
	}
	return env, nil
}

func Run(ctx context.Context, slug string, envFilePath string, noVerifyJWT *bool, importMapPath string, serveAll bool, fsys afero.Fs) error {
	if serveAll {
		return runServeAll(ctx, envFilePath, noVerifyJWT, importMapPath, fsys)
	}

	// 1. Sanity checks.
	{
		if err := utils.LoadConfigFS(fsys); err != nil {
			return err
		}
		if err := utils.AssertSupabaseDbIsRunning(); err != nil {
			return err
		}
		if err := utils.ValidateFunctionSlug(slug); err != nil {
			return err
		}
		if envFilePath != "" {
			if _, err := fsys.Stat(envFilePath); err != nil {
				return fmt.Errorf("Failed to read env file: %w", err)
			}
		}
		if importMapPath != "" {
			// skip
		} else if functionConfig, ok := utils.Config.Functions[slug]; ok && functionConfig.ImportMap != "" {
			if filepath.IsAbs(functionConfig.ImportMap) {
				importMapPath = functionConfig.ImportMap
			} else {
				importMapPath = filepath.Join(utils.SupabaseDirPath, functionConfig.ImportMap)
			}
		} else if f, err := fsys.Stat(utils.FallbackImportMapPath); err == nil && !f.IsDir() {
			importMapPath = utils.FallbackImportMapPath
		}
		if importMapPath != "" {
			if _, err := fsys.Stat(importMapPath); err != nil {
				return fmt.Errorf("Failed to read import map: %w", err)
			}
		}
	}

	// 2. Parse user defined env
	userEnv, err := ParseEnvFile(envFilePath)
	if err != nil {
		return err
	}

	// 3. Start relay.
	{
		_ = utils.Docker.ContainerRemove(ctx, utils.DenoRelayId, types.ContainerRemoveOptions{
			RemoveVolumes: true,
			Force:         true,
		})

		env := []string{
			"JWT_SECRET=" + utils.JWTSecret,
			"DENO_ORIGIN=http://localhost:8000",
		}
		verifyJWTEnv := "VERIFY_JWT=true"
		if noVerifyJWT == nil {
			if functionConfig, ok := utils.Config.Functions[slug]; ok && !*functionConfig.VerifyJWT {
				verifyJWTEnv = "VERIFY_JWT=false"
			}
		} else if *noVerifyJWT {
			verifyJWTEnv = "VERIFY_JWT=false"
		}
		env = append(env, verifyJWTEnv)

		cwd, err := os.Getwd()
		if err != nil {
			return err
		}

		binds := []string{filepath.Join(cwd, utils.FunctionsDir) + ":" + relayFuncDir + ":ro,z"}
		// If a import map path is explcitly provided, mount it as a separate file
		if importMapPath != "" {
			binds = append(binds, filepath.Join(cwd, importMapPath)+":"+customDockerImportMapPath+":ro,z")
		}
		if _, err := utils.DockerStart(
			ctx,
			container.Config{
				Image: utils.DenoRelayImage,
				Env:   append(env, userEnv...),
			},
			container.HostConfig{
				Binds: binds,
				// Allows containerized functions on Linux to reach host OS
				ExtraHosts: []string{"host.docker.internal:host-gateway"},
			},
			utils.DenoRelayId,
		); err != nil {
			return err
		}

		go func() {
			<-ctx.Done()
			if ctx.Err() != nil {
				utils.DockerRemove(utils.DenoRelayId)
			}
		}()
	}

	// 4. Start Function.
	localFuncDir := filepath.Join(utils.FunctionsDir, slug)
	localImportMapPath := filepath.Join(localFuncDir, "import_map.json")

	// We assume the image is always Linux, so path separator must always be `/`.
	// We can't use filepath.Join because it uses the path separator for the host system, which is `\` for Windows.
	dockerFuncPath := relayFuncDir + "/" + slug + "/index.ts"
	dockerImportMapPath := relayFuncDir + "/" + slug + "/import_map.json"

	if importMapPath != "" {
		localImportMapPath = importMapPath
		dockerImportMapPath = customDockerImportMapPath
	}

	denoCacheCmd := []string{"deno", "cache"}
	{
		if _, err := fsys.Stat(localImportMapPath); err == nil {
			denoCacheCmd = append(denoCacheCmd, "--import-map="+dockerImportMapPath)
		} else if errors.Is(err, os.ErrNotExist) {
			// skip
		} else {
			return fmt.Errorf("failed to check import_map.json for function %s: %w", slug, err)
		}
		denoCacheCmd = append(denoCacheCmd, dockerFuncPath)
	}

	fmt.Println("Starting " + utils.Bold(localFuncDir))
	if _, err := utils.DockerExecOnce(ctx, utils.DenoRelayId, userEnv, denoCacheCmd); err != nil {
		return err
	}

	{
		fmt.Println("Serving " + utils.Bold(localFuncDir))

		env := []string{
			"SUPABASE_URL=http://" + utils.KongId + ":8000",
			"SUPABASE_ANON_KEY=" + utils.AnonKey,
			"SUPABASE_SERVICE_ROLE_KEY=" + utils.ServiceRoleKey,
			"SUPABASE_DB_URL=postgresql://postgres:postgres@localhost:" + strconv.FormatUint(uint64(utils.Config.Db.Port), 10) + "/postgres",
		}

		denoRunCmd := []string{"deno", "run", "--no-check=remote", "--allow-all", "--watch", "--no-clear-screen", "--no-npm"}
		{
			if _, err := fsys.Stat(localImportMapPath); err == nil {
				denoRunCmd = append(denoRunCmd, "--import-map="+dockerImportMapPath)
			} else if errors.Is(err, os.ErrNotExist) {
				// skip
			} else {
				return fmt.Errorf("failed to check index.ts for function %s: %w", slug, err)
			}
			denoRunCmd = append(denoRunCmd, dockerFuncPath)
		}

		exec, err := utils.Docker.ContainerExecCreate(
			ctx,
			utils.DenoRelayId,
			types.ExecConfig{
				Env:          append(env, userEnv...),
				Cmd:          denoRunCmd,
				AttachStderr: true,
				AttachStdout: true,
			},
		)
		if err != nil {
			return err
		}

		resp, err := utils.Docker.ContainerExecAttach(ctx, exec.ID, types.ExecStartCheck{})
		if err != nil {
			return err
		}

		if _, err := stdcopy.StdCopy(os.Stdout, os.Stderr, resp.Reader); err != nil {
			return err
		}
	}

	fmt.Println("Stopped serving " + utils.Bold(localFuncDir))
	return nil
}

func runServeAll(ctx context.Context, envFilePath string, noVerifyJWT *bool, importMapPath string, fsys afero.Fs) error {
	// 1. Sanity checks.
	{
		if err := utils.LoadConfigFS(fsys); err != nil {
			return err
		}
		if err := utils.AssertSupabaseDbIsRunning(); err != nil {
			return err
		}
		if envFilePath != "" {
			if _, err := fsys.Stat(envFilePath); err != nil {
				return fmt.Errorf("Failed to read env file: %w", err)
			}
		}
		if importMapPath != "" {
			// skip
		} else if f, err := fsys.Stat(utils.FallbackImportMapPath); err == nil && !f.IsDir() {
			importMapPath = utils.FallbackImportMapPath
		}
		if importMapPath != "" {
			if _, err := fsys.Stat(importMapPath); err != nil {
				return fmt.Errorf("Failed to read import map: %w", err)
			}
		}
	}

	// 2. Parse user defined env
	userEnv, err := ParseEnvFile(envFilePath)
	if err != nil {
		return err
	}

	// 3. Start container
	{
		_ = utils.Docker.ContainerRemove(ctx, utils.DenoRelayId, types.ContainerRemoveOptions{
			RemoveVolumes: true,
			Force:         true,
		})

		env := []string{
			"JWT_SECRET=" + utils.JWTSecret,
			"SUPABASE_URL=http://" + utils.KongId + ":8000",
			"SUPABASE_ANON_KEY=" + utils.AnonKey,
			"SUPABASE_SERVICE_ROLE_KEY=" + utils.ServiceRoleKey,
			"SUPABASE_DB_URL=postgresql://postgres:postgres@localhost:" + strconv.FormatUint(uint64(utils.Config.Db.Port), 10) + "/postgres",
		}
		verifyJWTEnv := "VERIFY_JWT=true"
		if noVerifyJWT != nil {
			verifyJWTEnv = "VERIFY_JWT=false"
		}
		env = append(env, verifyJWTEnv)

		cwd, err := os.Getwd()
		if err != nil {
			return err
		}

		binds := []string{filepath.Join(cwd, utils.FunctionsDir) + ":" + relayFuncDir + ":ro,z"}
		// If a import map path is explcitly provided, mount it as a separate file
		if importMapPath != "" {
			binds = append(binds, filepath.Join(cwd, importMapPath)+":"+customDockerImportMapPath+":ro,z")
		}

		// bind deno cache directory
		cachePath, err := utils.GetDenoCachePath()
		if err != nil {
			return err
		}
		binds = append(binds, cachePath+":/root/.cache/deno:rw,z")

		containerID, err := utils.DockerStart(
			ctx,
			container.Config{
				Image:        utils.EdgeRuntimeImage,
				Env:          append(env, userEnv...),
				Cmd:          []string{"start", "--dir", relayFuncDir, "-p", "8081"},
				OpenStdin:    true,
				AttachStdin:  true,
				AttachStderr: true,
				AttachStdout: true,
				Tty:          true,
			},
			container.HostConfig{
				Binds: binds,
			},
			utils.DenoRelayId,
		)
		if err != nil {
			return err
		}

		fmt.Println("Serving " + utils.Bold(utils.FunctionsDir))

		// TODO: pipe the OS signals to the container
		resp, err := utils.Docker.ContainerAttach(ctx, containerID, types.ContainerAttachOptions{
			Stream: true,
			Stdin:  true,
			Stdout: true,
			Stderr: true,
			Logs:   true,
		})
		if err != nil {
			return err
		}

		go func() {
			_, _ = io.Copy(os.Stdout, resp.Reader)
		}()

		statusCh, errCh := utils.Docker.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
		select {
		case err := <-errCh:
			if err != nil {
				return err
			}
		case <-statusCh:
		}

		go func() {
			<-ctx.Done()
			if ctx.Err() != nil {
				utils.DockerRemove(utils.DenoRelayId)
			}
		}()
	}

	fmt.Println("Stopped serving " + utils.Bold(utils.FunctionsDir))
	return nil

}
