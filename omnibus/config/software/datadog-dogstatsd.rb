# Unless explicitly stated otherwise all files in this repository are licensed
# under the Apache License Version 2.0.
# This product includes software developed at Datadog (https:#www.datadoghq.com/).
# Copyright 2016-present Datadog, Inc.
require 'pathname'

name 'datadog-dogstatsd'

skip_transitive_dependency_licensing true

source path: '..',
       options: {
         exclude: ["**/.cache/**/*"],
       }
relative_path 'src/github.com/DataDog/datadog-agent'

build do
  license :project_license

  # set GOPATH on the omnibus source dir for this software
  gopath = Pathname.new(project_dir) + '../../../..'
  env = {
    'GOPATH' => gopath.to_path,
    'PATH' => ["#{gopath.to_path}/bin", ENV['PATH']].join(File::PATH_SEPARATOR),
  }

  unless ENV["OMNIBUS_GOMODCACHE"].nil? || ENV["OMNIBUS_GOMODCACHE"].empty?
    gomodcache = Pathname.new(ENV["OMNIBUS_GOMODCACHE"])
    env["GOMODCACHE"] = gomodcache.to_path
  end

  # we assume the go deps are already installed before running omnibus
  command "invoke dogstatsd.build", env: env, :live_stream => Omnibus.logger.live_stream(:info)

  mkdir "#{install_dir}/etc/datadog-dogstatsd"

  # move around bin and config files
  if windows_target?
    mkdir "#{Omnibus::Config.source_dir()}/datadog-agent/src/github.com/DataDog/datadog-agent/bin/agent"
    copy 'bin/dogstatsd/dogstatsd.exe', "#{Omnibus::Config.source_dir()}/datadog-agent/src/github.com/DataDog/datadog-agent/bin/agent"
  else
    copy 'bin/dogstatsd/dogstatsd', "#{install_dir}/bin"
  end
  move 'bin/dogstatsd/dist/dogstatsd.yaml', "#{install_dir}/etc/datadog-dogstatsd/dogstatsd.yaml.example"

  if linux_target?
    if debian_target?
      install_target = "//packages/dogstatsd/linux:install_debian"
    elsif redhat_target? || suse_target?
      install_target = "//packages/dogstatsd/linux:install_redhat"
    end
    command_on_repo_root "bazelisk run --//:install_dir=#{install_dir} -- #{install_target} --destdir=/",
      :live_stream => Omnibus.logger.live_stream(:info)

    project.extra_package_file '/etc/init/datadog-dogstatsd.conf'
    project.extra_package_file '/lib/systemd/system/datadog-dogstatsd.service'
  end
end
