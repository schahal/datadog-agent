# Unless explicitly stated otherwise all files in this repository are licensed
# under the Apache License Version 2.0.
# This product includes software developed at Datadog (https:#www.datadoghq.com/).
# Copyright 2018 Datadog, Inc.

require "./lib/ostools.rb"
require 'pathname'

name "datadog-process-agent"

dependency "datadog-agent"

process_agent_version = ENV['PROCESS_AGENT_VERSION']
if process_agent_version.nil? || process_agent_version.empty?
  process_agent_version = 'master'
end
default_version process_agent_version

url = "https://s3.amazonaws.com/datad0g-process-agent/"

build do
  ship_license "https://raw.githubusercontent.com/DataDog/datadog-process-agent/#{version}/LICENSE"
  if windows?
    binary = "process-agent-windows-#{version}.exe"
    target_binary = "process-agent.exe"
    curl_cmd = "powershell -Command wget -OutFile #{binary} #{url}#{binary}"
    command curl_cmd
    command "mv #{binary} #{install_dir}/bin/agent/#{target_binary}"
  else
    target_binary = "process-agent"
    if arm?
      # TODO: Build the process-agent on arm and curl it from the agent repo
      source git: 'https://github.com/DataDog/datadog-process-agent.git'
      relative_path 'src/github.com/DataDog/datadog-process-agent'
      # set GOPATH on the omnibus source dir for this software
      gopath = Pathname.new(project_dir) + '../../../..'
      env = {
        'GOPATH' => gopath.to_path,
        'PATH' => "#{gopath.to_path}/bin:#{ENV['PATH']}",
      }

      block do
        # defer compilation step in a block to allow getting the project's build version, which is populated
        # only once the software that the project takes its version from (i.e. `datadog-agent`) has finished building
        env['PROCESS_AGENT_VERSION'] = project.build_version.gsub(/[^0-9\.]/, '') # used by gorake.rb in the process-agent, only keep digits and dots

        # build process-agent
        command "rake deps", :env => env
        command "rake build", :env => env

        # copy binary
        copy "#{project_dir}/#{target_binary}", "#{install_dir}/embedded/bin"
      end
    else
      binary = "process-agent-amd64-#{version}"
      curl_cmd = "curl -f #{url}#{binary} -o #{binary}"
      command curl_cmd
      command "chmod +x #{binary}"
      command "mv #{binary} #{install_dir}/embedded/bin/#{target_binary}"

      # network-tracer versions will always be the same on both process-agent and network-tracer
      ship_license "https://raw.githubusercontent.com/DataDog/tcptracer-bpf/#{version}/LICENSE.network-tracer"
      binary = "network-tracer-amd64-#{version}"
      target_binary = "network-tracer"
      curl_cmd = "curl -f #{url}#{binary} -o #{binary}"
      command curl_cmd
      command "chmod +x #{binary}"
      command "mv #{binary} #{install_dir}/embedded/bin/#{target_binary}"
    end
  end
end
