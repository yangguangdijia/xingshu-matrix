package provision

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ansible"
	"gopkg.in/yaml.v3"
)

const installPlaybook = `---
- name: Install K3s node
  hosts: target
  gather_facts: false
  tasks:
    - name: Run the preflight-approved installer
      ansible.builtin.raw: "printf '%s' '{{ matrix_script_b64 }}' | base64 -d | sh"
      changed_when: true
      no_log: true
`

type ansibleInventory struct {
	All struct {
		Hosts map[string]ansibleHost `yaml:"hosts"`
	} `yaml:"all"`
}

type ansibleHost struct {
	Address  string `yaml:"ansible_host"`
	Port     int    `yaml:"ansible_port"`
	User     string `yaml:"ansible_user"`
	Password string `yaml:"ansible_password"`
	Become   bool   `yaml:"ansible_become"`
}

func installWithAnsible(ctx context.Context, host Host, hostKey, script string) error {
	inventoryData, err := buildAnsibleInventory(host)
	if err != nil {
		return errors.New("could not prepare Ansible inventory")
	}
	varsData, err := json.Marshal(map[string]string{
		"matrix_script_b64": base64.StdEncoding.EncodeToString([]byte(script)),
	})
	if err != nil {
		return errors.New("could not prepare Ansible variables")
	}
	return ansible.Run(ctx, ansible.Input{
		Inventory:  inventoryData,
		Vars:       varsData,
		KnownHosts: []byte(knownHostsEntry(host, hostKey)),
		Playbook:   []byte(installPlaybook),
	})
}

func buildAnsibleInventory(host Host) ([]byte, error) {
	inventory := ansibleInventory{}
	inventory.All.Hosts = map[string]ansibleHost{
		"target": {
			Address: host.IP, Port: host.Port, User: host.Username,
			Password: host.Password, Become: host.Username != "root",
		},
	}
	return yaml.Marshal(inventory)
}

func knownHostsEntry(host Host, key string) string {
	name := host.IP
	if host.Port != 22 {
		name = fmt.Sprintf("[%s]:%d", host.IP, host.Port)
	}
	return name + " " + key + "\n"
}
