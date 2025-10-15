# CAST AI Castware Operator

An operator that manages Castware components.

## How to run in a local environment
1. Generate an api key in console
2. Set the environment variable `API_KEY` with the generated key
3. Set the `API_URL` environment variable to the api url of the environment where you generated the api key
4. Run `make run-local`

Sample custom resources for cluster and component are generated in `local/samples`.

### How to use local helm registry

#### Pushing the chart to the local registry
1. cd to the chart directory
2. Run `helm package ./castai-castware-operator`
3. Run `helm repo add local http://localhost:5001/helm-charts`
4. Run `curl --data-binary "@castware-operator-[myversion].tgz" http://localhost:5001/helm-charts/api/charts` replacing 
   `[myversion]` with the version of the chart (for example 0.0.7)
5. Run `helm repo update`

#### Test self upgrade from helm registry
1. Push at least two versions to the local registry (change `version` and `appVersion` in `Chart.yaml` before running `helm package`)
2. Install the operator from the local registry
3. Change `helmRepoURL` in cluster CR to `http://localhost:5001` to `http://chartmuseum.registry.svc.cluster.local:8080/helm-charts`
4. Run self upgrade from a job manifest or an action


### Local environment cleanup
1. Run `make updeploy`

Ignore the error about the missing `castware-operator-controller-manager` deployment.
