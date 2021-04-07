#!/bin/bash

# Copyright 2019 The Kubernetes Authors.
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

function cleanup {
  echo 'pkill -f azurediskplugin'
  pkill -f azurediskplugin
}

trap cleanup EXIT

endpoint='tcp://127.0.0.1:10000'
if [[ "$#" -gt 0 ]]; then
  endpoint="$1"
fi

_output/azurediskplugin --endpoint "$endpoint" --nodeid "$node" -v=5 &

echo "Begin to run integration test on $cloud..."

test/integration/run-test.sh "$endpoint" "$nodeid" "$cloud" "$version"