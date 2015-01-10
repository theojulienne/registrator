package main

import (
	"log"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"

	dockerapi "github.com/fsouza/go-dockerclient"
)

type PublishedPort struct {
	HostPort    string
	HostIP      string
	ExposedPort string
	PortType    string
	Container   *dockerapi.Container
}

type Service struct {
	ID    string
	Name  string
	Port  int
	IP    string
	Tags  []string
	Attrs map[string]string

	pp PublishedPort
}

func NewService(port PublishedPort, isgroup bool) *Service {
	container := port.Container
	defaultName := strings.Split(path.Base(container.Config.Image), ":")[0]
	if isgroup {
		defaultName = defaultName + "-" + port.ExposedPort
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = port.HostIP
	} else {
		if port.HostIP == "0.0.0.0" {
			ip, err := net.ResolveIPAddr("ip", hostname)
			if err == nil {
				port.HostIP = ip.String()
			}
		}
	}

	if *hostIp != "" {
		port.HostIP = *hostIp
	}

	metadata := serviceMetaData(container.Config.Env, port.ExposedPort)

	ignore := mapdefault(metadata, "ignore", "")
	if ignore != "" {
		return nil
	}

	service := new(Service)
	service.pp = port
	service.ID = hostname + ":" + container.Name[1:] + ":" + port.ExposedPort
	service.Name = mapdefault(metadata, "name", defaultName)
	p, _ := strconv.Atoi(port.HostPort)
	service.Port = p
	service.IP = port.HostIP

	service.Tags = make([]string, 0)
	tags := mapdefault(metadata, "tags", "")
	if tags != "" {
		service.Tags = append(service.Tags, strings.Split(tags, ",")...)
	}
	if port.PortType == "udp" {
		service.Tags = append(service.Tags, "udp")
		service.ID = service.ID + ":udp"
	}

	id := mapdefault(metadata, "id", "")
	if id != "" {
		service.ID = id
	}

	delete(metadata, "id")
	delete(metadata, "tags")
	delete(metadata, "name")
	service.Attrs = metadata

	return service
}

func serviceMetaData(env []string, port string) map[string]string {
	metadata := make(map[string]string)
	for _, kv := range env {
		kvp := strings.SplitN(kv, "=", 2)
		if strings.HasPrefix(kvp[0], "SERVICE_") && len(kvp) > 1 {
			key := strings.ToLower(strings.TrimPrefix(kvp[0], "SERVICE_"))
			portkey := strings.SplitN(key, "_", 2)
			_, err := strconv.Atoi(portkey[0])
			if err == nil && len(portkey) > 1 {
				if portkey[0] != port {
					continue
				}
				metadata[portkey[1]] = kvp[1]
			} else {
				metadata[key] = kvp[1]
			}
		}
	}
	return metadata
}

type RegistryBridge struct {
	sync.Mutex
	docker   *dockerapi.Client
	registry ServiceRegistry
	services map[string][]*Service
}

func (b *RegistryBridge) Add(containerId string) {
	b.Lock()
	defer b.Unlock()
	container, err := b.docker.InspectContainer(containerId)
	if err != nil {
		log.Println("registrator: unable to inspect container:", containerId[:12], err)
		return
	}

	ports := make([]PublishedPort, 0)
	for port, published := range container.NetworkSettings.Ports {
		p := strings.Split(string(port), "/")
		if len(published) > 0 && !*registerInternalAddress {
			ports = append(ports, PublishedPort{
				HostPort:    published[0].HostPort,
				HostIP:      published[0].HostIp,
				ExposedPort: p[0],
				PortType:    p[1],
				Container:   container,
			})
		} else if len(published) > 0 || !*registerExposedPorts {
			ports = append(ports, PublishedPort{
				HostPort:    p[0],
				HostIP:      container.NetworkSettings.IPAddress,
				ExposedPort: p[0],
				PortType:    p[1],
				Container:   container,
			})
		}
	}

	if len(ports) == 0 {
		log.Println("registrator: ignored:", container.ID[:12], "no published ports")
		return
	}

	for _, port := range ports {
		service := NewService(port, len(ports) > 1)
		if service == nil {
			log.Println("registrator: ignored:", container.ID[:12], "service on port", port.ExposedPort)
			continue
		}
		err := retry(func() error {
			return b.registry.Register(service)
		})
		if err != nil {
			log.Println("registrator: unable to register service:", service, err)
			continue
		}
		b.services[container.ID] = append(b.services[container.ID], service)
		log.Println("registrator: added:", container.ID[:12], service.ID)
	}
}

func (b *RegistryBridge) Remove(containerId string) {
	b.Lock()
	defer b.Unlock()
	for _, service := range b.services[containerId] {
		err := retry(func() error {
			return b.registry.Deregister(service)
		})
		if err != nil {
			log.Println("registrator: unable to deregister service:", service.ID, err)
			continue
		}
		log.Println("registrator: removed:", containerId[:12], service.ID)
	}
	delete(b.services, containerId)
}
