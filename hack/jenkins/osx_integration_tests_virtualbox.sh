#!/bin/bash

# Copyright 2016 The Kubernetes Authors All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.


# This script runs the integration tests on an OSX machine for the Virtualbox Driver

# The script expects the following env variabls:
# MINIKUBE_LOCATION: GIT_COMMIT from upstream build.
# COMMIT: Actual commit ID from upstream build
# EXTRA_BUILD_ARGS (optional): Extra args to be passed into the minikube integrations tests
# access_token: The Github API access token. Injected by the Jenkins credential provider. 


set -e
chmod +x out/e2e-darwin-amd64
chmod +x out/minikube-darwin-amd64
cp -r out/testdata ./


./out/minikube-darwin-amd64 delete || true
rm -rf $HOME/.minikube || true

# Allow this to fail, we'll switch on the return code below.
set +e
out/e2e-darwin-amd64 -minikube-args="--vm-driver=virtualbox --cpus=4 --show-libmachine-logs --v=100 ${EXTRA_BUILD_ARGS}" -test.v -test.timeout=30m -binary=out/minikube-darwin-amd64
result=$?
set -e

if [[ $result -eq 0 ]]; then
  status="success"
else
  status="failure"
fi

set +x
target_url="https://storage.googleapis.com/minikube-builds/logs/${MINIKUBE_LOCATION}/OSX-Virtualbox.txt"
curl "https://api.github.com/repos/kubernetes/minikube/statuses/${COMMIT}?access_token=$access_token" \
  -H "Content-Type: application/json" \
  -X POST \
  -d "{\"state\": \"$status\", \"description\": \"Jenkins\", \"target_url\": \"$target_url\", \"context\": \"OSX-VirtualBox\"}"
set -x

exit $result
