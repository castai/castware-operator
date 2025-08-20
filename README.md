# CAST AI Castware Operator

An operator that manages Castware components.

## How to run in a local environment
1. Generate an api key in console
2. Set the environment variable `API_KEY` with the generated key
3. Set the `API_URL` environment variable to the api url of the environment where you generated the api key
4. Run `make run-local`

Sample custom resources for cluster and component are generated in `local/samples`.

### Local environment cleanup
1. Run `make updeploy`

Ignore the error about the missing `castware-operator-controller-manager` deployment.
