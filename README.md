# HYDRA

This is a modular framework for implementing, deploying and testing a distributed ordering service.

Hydra is a Multi-BFT protocol without global ordering.


## Installation
### Cloning the repository
Create a GOPATH directory and make sure you are the owner of it:

`sudo mkdir -p /opt/gopath/`

`sudo chown -R $user:$group  /opt/gopath/`

where `$user` and `$group` your user and group respectively.

Create a directory to clone the repository into:

`mkdir -p /opt/gopath/src/github.com/hyperledger-labs/`

Clone this repository unter the directory you created:

`cd /opt/gopath/src/github.com/hyperledger-labs/`

`git clone https://github.com/hyperledger-labs/hydra.git`

Checkout the`research-iss` branch.

### Installing Dependencies
With `/opt/gopath/src/github.com/hyperledger-labs/hydra` as working directory, go to the deployment directory:

`cd deployment`

Configure the `user` and `group` in `vars.sh`

To install Golang and requirements: 

`source scripts/install-local.sh`

**NOTE**: The `install-local.sh` script, among other dependencies, installs `Go` in the home directory, sets GOPATH to `/opt/gopath/bin/` and edits `~/.bashrc`.

The default path to the repository is set to: `/opt/gopath/src/github.com/hyperledger-labs/hydra/`.


### ISS Installation
The `run-protoc.sh` script needs to be run from the project root directory (i.e. `hydra`) before compiling the Go
files. 

**IMPORTANT**: go modules are not supported. Disable with the command: `export GO111MODULE=off` before installation.

Compile and install the go code by running `go install ./...` from the project root directory.


## Deployment & Permformance Metrics
Detailed instructions can be found  [here](https://github.com/ShaokangXie/hydra/tree/main/deployment).


