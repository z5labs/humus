# Calling dependencies

A service calls each of its dependencies with a client it builds in `Init`.
These pages cover the two clients humus has, in this order: HTTP services
with `httpclient`, then gRPC services with `grpcclient`. Both retry what is
safe to retry, put a circuit breaker in front of each dependency, and let the
end user's `Classify` decide what an answer means.

Next: [Calling HTTP services](/humus/pr-preview/pr-1308/dependencies/http/)
