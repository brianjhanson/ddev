package ddevapp

import (
	"fmt"
	"github.com/Masterminds/sprig/v3"
	"github.com/drud/ddev/pkg/dockerutil"
	"github.com/drud/ddev/pkg/exec"
	"github.com/drud/ddev/pkg/fileutil"
	"github.com/drud/ddev/pkg/globalconfig"
	"github.com/drud/ddev/pkg/nodeps"
	"github.com/drud/ddev/pkg/util"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"
)

type TraefikRouting struct {
	ExternalHostnames   []string
	ExternalPort        string
	InternalServiceName string
	InternalServicePort string
	HTTPS               bool
}

// detectAppRouting reviews the configured services and uses their
// VIRTUAL_HOST and HTTP(S)_EXPOSE environment variables to set up routing
// for the project
func detectAppRouting(app *DdevApp) ([]TraefikRouting, error) {
	// app.ComposeYaml["services"];
	var table []TraefikRouting
	if services, ok := app.ComposeYaml["services"]; ok {
		for serviceName, s := range services.(map[interface{}]interface{}) {
			service := s.(map[interface{}]interface{})
			if env, ok := service["environment"].(map[interface{}]interface{}); ok {
				var virtualHost string
				var ok bool
				if virtualHost, ok = env["VIRTUAL_HOST"].(string); ok {
					util.Debug("VIRTUAL_HOST=%v for %s", virtualHost, serviceName)
				}
				hostnames := strings.Split(virtualHost, ",")
				if httpExpose, ok := env["HTTP_EXPOSE"].(string); ok {
					util.Debug("HTTP_EXPOSE=%v for %s", httpExpose, serviceName)
					routeEntries, err := processHTTPExpose(serviceName.(string), httpExpose, false, hostnames)
					if err != nil {
						return nil, err
					}
					table = append(table, routeEntries...)
				}

				if httpsExpose, ok := env["HTTPS_EXPOSE"].(string); ok {
					util.Debug("HTTPS_EXPOSE=%v for %s", httpsExpose, serviceName)
					routeEntries, err := processHTTPExpose(serviceName.(string), httpsExpose, true, hostnames)
					if err != nil {
						return nil, err
					}
					table = append(table, routeEntries...)
				}
			}
		}
	}
	return table, nil
}

// processHTTPExpose creates routing table entry from VIRTUAL_HOST and HTTP(S)_EXPOSE
// environment variables
func processHTTPExpose(serviceName string, httpExpose string, isHTTPS bool, externalHostnames []string) ([]TraefikRouting, error) {
	var routingTable []TraefikRouting
	portPairs := strings.Split(httpExpose, ",")
	for _, portPair := range portPairs {
		ports := strings.Split(portPair, ":")
		if len(ports) == 0 || len(ports) > 2 {
			util.Warning("Skipping bad HTTP_EXPOSE port pair spec %s for service %s", portPair, serviceName)
			continue
		}
		if len(ports) == 1 {
			ports = append(ports, ports[0])
		}
		routingTable = append(routingTable, TraefikRouting{ExternalHostnames: externalHostnames, ExternalPort: ports[0], InternalServiceName: serviceName, InternalServicePort: ports[1], HTTPS: isHTTPS})
	}
	return routingTable, nil
}

// pushGlobalTraefikConfig pushes the config into ddev-global-cache
func pushGlobalTraefikConfig() error {
	globalTraefikDir := filepath.Join(globalconfig.GetGlobalDdevDir(), "traefik")
	err := os.MkdirAll(globalTraefikDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create global .ddev/traefik directory: %v", err)
	}
	sourceCertsPath := filepath.Join(globalTraefikDir, "certs")
	// SourceConfigDir for dynamic config
	sourceConfigDir := filepath.Join(globalTraefikDir, "config")
	targetCertsPath := path.Join("/mnt/ddev-global-cache/traefik/certs")

	err = os.MkdirAll(sourceCertsPath, 0755)
	if err != nil {
		return fmt.Errorf("failed to create global traefik certs dir: %v", err)
	}
	err = os.MkdirAll(sourceConfigDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create global traefik config dir: %v", err)
	}

	// Assume that the #ddev-generated exists in file unless it doesn't
	sigExists := true
	for _, pemFile := range []string{"default_cert.crt", "default_key.key"} {
		origFile := filepath.Join(sourceCertsPath, pemFile)
		if fileutil.FileExists(origFile) {
			// Check to see if file has #ddev-generated in it, meaning we can recreate it.
			sigExists, err = fileutil.FgrepStringInFile(origFile, nodeps.DdevFileSignature)
			if err != nil {
				return err
			}
			// If either of the files has #ddev-generated, we will respect both
			if !sigExists {
				break
			}
		}
	}
	if sigExists && globalconfig.DdevGlobalConfig.MkcertCARoot != "" {
		c := []string{"--cert-file", filepath.Join(sourceCertsPath, "default_cert.crt"), "--key-file", filepath.Join(sourceCertsPath, "default_key.key"), "127.0.0.1", "localhost", "*.ddev.local", "ddev-router", "ddev-router.ddev", "ddev-router.ddev_default", "*.ddev.site"}
		if globalconfig.DdevGlobalConfig.ProjectTldGlobal != "" {
			c = append(c, "*."+globalconfig.DdevGlobalConfig.ProjectTldGlobal)
		}

		out, err := exec.RunHostCommand("mkcert", c...)
		if err != nil {
			util.Failed("failed to create global mkcert certificate, check mkcert operation: %v", out)
		}

		// Prepend #ddev-generated in generated crt and key files
		for _, pemFile := range []string{"default_cert.crt", "default_key.key"} {
			origFile := filepath.Join(sourceCertsPath, pemFile)

			contents, err := fileutil.ReadFileIntoString(origFile)
			if err != nil {
				return fmt.Errorf("failed to read file %v: %v", origFile, err)
			}
			contents = nodeps.DdevFileSignature + "\n" + contents
			err = fileutil.TemplateStringToFile(contents, nil, origFile)
			if err != nil {
				return err
			}
		}
	}

	type traefikData struct {
		App              *DdevApp
		Hostnames        []string
		PrimaryHostname  string
		TargetCertsPath  string
		RouterPorts      []string
		UseLetsEncrypt   bool
		LetsEncryptEmail string
	}
	templateData := traefikData{
		TargetCertsPath:  targetCertsPath,
		RouterPorts:      determineRouterPorts(),
		UseLetsEncrypt:   globalconfig.DdevGlobalConfig.UseLetsEncrypt,
		LetsEncryptEmail: globalconfig.DdevGlobalConfig.LetsEncryptEmail,
	}

	traefikYamlFile := filepath.Join(sourceConfigDir, "default_config.yaml")
	sigExists = true
	//TODO: Systematize this checking-for-signature, allow an arg to skip if empty
	fi, err := os.Stat(traefikYamlFile)
	// Don't use simple fileutil.FileExists() because of the danger of an empty file
	if err == nil && fi.Size() > 0 {
		// Check to see if file has #ddev-generated in it, meaning we can recreate it.
		sigExists, err = fileutil.FgrepStringInFile(traefikYamlFile, nodeps.DdevFileSignature)
		if err != nil {
			return err
		}
	}
	if !sigExists {
		util.Debug("Not creating %s because it exists and is managed by user", traefikYamlFile)
	} else {
		f, err := os.Create(traefikYamlFile)
		if err != nil {
			util.Failed("failed to create traefik config file: %v", err)
		}
		t, err := template.New("traefik_global_config_template.yaml").Funcs(sprig.TxtFuncMap()).ParseFS(bundledAssets, "traefik_global_config_template.yaml")
		if err != nil {
			return fmt.Errorf("could not create template from traefik_global_config_template.yaml: %v", err)
		}

		err = t.Execute(f, templateData)
		if err != nil {
			return fmt.Errorf("could not parse traefik_global_config_template.yaml with templatedate='%v':: %v", templateData, err)
		}
	}

	// sourceConfigDir for static config
	sourceConfigDir = globalTraefikDir
	traefikYamlFile = filepath.Join(sourceConfigDir, "static_config.yaml")
	sigExists = true
	//TODO: Systematize this checking-for-signature, allow an arg to skip if empty
	fi, err = os.Stat(traefikYamlFile)
	// Don't use simple fileutil.FileExists() because of the danger of an empty file
	if err == nil && fi.Size() > 0 {
		// Check to see if file has #ddev-generated in it, meaning we can recreate it.
		sigExists, err = fileutil.FgrepStringInFile(traefikYamlFile, nodeps.DdevFileSignature)
		if err != nil {
			return err
		}
	}
	if !sigExists {
		util.Debug("Not creating %s because it exists and is managed by user", traefikYamlFile)
	} else {
		f, err := os.Create(traefikYamlFile)
		if err != nil {
			util.Failed("failed to create traefik config file: %v", err)
		}
		t, err := template.New("traefik_static_config_template.yaml").Funcs(sprig.TxtFuncMap()).ParseFS(bundledAssets, "traefik_static_config_template.yaml")
		if err != nil {
			return fmt.Errorf("could not create template from traefik_static_config_template.yaml: %v", err)
		}

		err = t.Execute(f, templateData)
		if err != nil {
			return fmt.Errorf("could not parse traefik_global_config_template.yaml with templatedate='%v':: %v", templateData, err)
		}
	}
	uid, _, _ := util.GetContainerUIDGid()

	err = dockerutil.CopyIntoVolume(globalTraefikDir, "ddev-global-cache", "traefik", uid, "", false)
	if err != nil {
		return fmt.Errorf("failed to copy global traefik config into docker volume ddev-global-cache/traefik: %v", err)
	}
	util.Debug("Copied global traefik config in %s to ddev-global-cache/traefik", sourceCertsPath)

	return nil
}

// configureTraefikForApp configures the dynamic configuration and creates cert+key
// in .ddev/traefik
func configureTraefikForApp(app *DdevApp) error {
	routingTable, err := detectAppRouting(app)
	if err != nil {
		return err
	}
	hostnames := app.GetHostnames()
	// There can possibly be VIRTUAL_HOST entries which are not configured hostnames.
	for _, r := range routingTable {
		if r.ExternalHostnames != nil {
			hostnames = append(hostnames, r.ExternalHostnames...)
		}
	}
	hostnames = util.SliceToUniqueSlice(&hostnames)
	projectTraefikDir := app.GetConfigPath("traefik")
	err = os.MkdirAll(projectTraefikDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create .ddev/traefik directory: %v", err)
	}
	sourceCertsPath := filepath.Join(projectTraefikDir, "certs")
	sourceConfigDir := filepath.Join(projectTraefikDir, "config")
	targetCertsPath := path.Join("/mnt/ddev-global-cache/traefik/certs")
	customCertsPath := app.GetConfigPath("custom_certs")

	err = os.MkdirAll(sourceCertsPath, 0755)
	if err != nil {
		return fmt.Errorf("failed to create traefik certs dir: %v", err)
	}
	err = os.MkdirAll(sourceConfigDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create traefik config dir: %v", err)
	}

	baseName := filepath.Join(sourceCertsPath, app.Name)
	// Assume that the #ddev-generated exists in file unless it doesn't
	sigExists := true
	for _, pemFile := range []string{app.Name + ".crt", app.Name + ".key"} {
		origFile := filepath.Join(sourceCertsPath, pemFile)
		if fileutil.FileExists(origFile) {
			// Check to see if file has #ddev-generated in it, meaning we can recreate it.
			sigExists, err = fileutil.FgrepStringInFile(origFile, nodeps.DdevFileSignature)
			if err != nil {
				return err
			}
			// If either of the files has #ddev-generated, we will respect both
			if !sigExists {
				break
			}
		}
	}
	// Assuming the certs don't exist, or they have #ddev-generated so can be replaced, create them
	// But not if we don't have mkcert already set up.
	if sigExists && globalconfig.DdevGlobalConfig.MkcertCARoot != "" {
		c := []string{"--cert-file", baseName + ".crt", "--key-file", baseName + ".key", "*.ddev.site", "127.0.0.1", "localhost", "*.ddev.local", "ddev-router", "ddev-router.ddev", "ddev-router.ddev_default"}
		c = append(c, hostnames...)
		if app.ProjectTLD != nodeps.DdevDefaultTLD {
			c = append(c, "*."+app.ProjectTLD)
		}
		out, err := exec.RunHostCommand("mkcert", c...)
		if err != nil {
			util.Failed("failed to create certificates for project, check mkcert operation: %v; err=%v", out, err)
		}

		// Prepend #ddev-generated in generated crt and key files
		for _, pemFile := range []string{app.Name + ".crt", app.Name + ".key"} {
			origFile := filepath.Join(sourceCertsPath, pemFile)

			contents, err := fileutil.ReadFileIntoString(origFile)
			if err != nil {
				return fmt.Errorf("failed to read file %v: %v", origFile, err)
			}
			contents = nodeps.DdevFileSignature + "\n" + contents
			err = fileutil.TemplateStringToFile(contents, nil, origFile)
			if err != nil {
				return err
			}
		}
	}

	type traefikData struct {
		App             *DdevApp
		Hostnames       []string
		PrimaryHostname string
		TargetCertsPath string
		RoutingTable    []TraefikRouting
		UseLetsEncrypt  bool
	}
	templateData := traefikData{
		App:             app,
		Hostnames:       []string{},
		PrimaryHostname: app.GetHostname(),
		TargetCertsPath: targetCertsPath,
		RoutingTable:    routingTable,
		UseLetsEncrypt:  globalconfig.DdevGlobalConfig.UseLetsEncrypt,
	}

	// Convert wildcards like `*.<anything>` to `.*\.anything`
	for _, hostname := range app.GetHostnames() {
		if strings.HasPrefix(hostname, `*.`) {
			hostname = `{subdomain:.+}` + strings.TrimPrefix(hostname, `*`)
		}
		templateData.Hostnames = append(templateData.Hostnames, hostname)
	}

	traefikYamlFile := filepath.Join(sourceConfigDir, app.Name+".yaml")
	sigExists = true
	fi, err := os.Stat(traefikYamlFile)
	// Don't use simple fileutil.FileExists() because of the danger of an empty file
	if err == nil && fi.Size() > 0 {
		// Check to see if file has #ddev-generated in it, meaning we can recreate it.
		sigExists, err = fileutil.FgrepStringInFile(traefikYamlFile, nodeps.DdevFileSignature)
		if err != nil {
			return err
		}
	}
	if !sigExists {
		util.Debug("Not creating %s because it exists and is managed by user", traefikYamlFile)
	} else {
		f, err := os.Create(traefikYamlFile)
		if err != nil {
			util.Failed("failed to create traefik config file: %v", err)
		}
		t, err := template.New("traefik_config_template.yaml").Funcs(sprig.TxtFuncMap()).ParseFS(bundledAssets, "traefik_config_template.yaml")
		if err != nil {
			return fmt.Errorf("could not create template from traefik_config_template.yaml: %v", err)
		}

		err = t.Execute(f, templateData)
		if err != nil {
			return fmt.Errorf("could not parse traefik_config_template.yaml with templatedate='%v':: %v", templateData, err)
		}
	}

	uid, _, _ := util.GetContainerUIDGid()

	err = dockerutil.CopyIntoVolume(projectTraefikDir, "ddev-global-cache", "traefik", uid, "", false)
	if err != nil {
		util.Warning("failed to copy traefik into docker volume ddev-global-cache/traefik: %v", err)
	} else {
		util.Debug("Copied traefik certs in %s to ddev-global-cache/traefik", sourceCertsPath)
	}
	if fileutil.FileExists(filepath.Join(customCertsPath, fmt.Sprintf("%s.crt", app.Name))) {
		err = dockerutil.CopyIntoVolume(app.GetConfigPath("custom_certs"), "ddev-global-cache", "traefik/certs", uid, "", false)
		if err != nil {
			util.Warning("failed copying custom certs into docker volume ddev-global-cache/traefik/certs: %v", err)
		} else {
			util.Debug("Copied custom certs in %s to ddev-global-cache/traefik", sourceCertsPath)
		}
	}
	return nil
}
