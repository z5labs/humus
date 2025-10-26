---
title: FAQ & Troubleshooting
description: Common questions and solutions
weight: 7
type: docs
---

# FAQ & Troubleshooting

Common questions, issues, and solutions for Humus applications.

## Sections

- [Frequently Asked Questions]({{< ref "faq" >}}) - Common questions about Humus
- [Troubleshooting]({{< ref "troubleshooting" >}}) - Common issues and solutions
- [Best Practices]({{< ref "best-practices" >}}) - Recommended patterns

## Quick Answers

### General

**Q: What's the difference between Humus and Bedrock?**

A: Bedrock is the foundation framework providing application lifecycle management. Humus builds on Bedrock to provide service-specific patterns for REST, gRPC, and Job services.

**Q: Do I need to know Bedrock to use Humus?**

A: No. Humus abstracts Bedrock's complexity. You only need to know Humus APIs for most use cases.

**Q: Can I use Humus with existing Go code?**

A: Yes. Humus is compatible with standard Go HTTP middleware, gRPC interceptors, and other Go libraries.

### REST Services

**Q: Can I use Humus with my existing chi router?**

A: Humus uses chi internally, so chi middleware is compatible. However, you should use Humus's routing APIs for full OpenAPI support.

**Q: How do I add custom middleware?**

A: See [Advanced Topics - Middleware]({{< ref "/advanced/middleware" >}}).

**Q: Can I disable OpenAPI generation?**

A: Currently, OpenAPI generation is always enabled. It adds minimal overhead.

### gRPC Services

**Q: Can I use Protocol Buffers v2?**

A: Humus works with both proto2 and proto3. We recommend proto3 for new projects.

**Q: How do I add custom interceptors?**

A: See [Advanced Topics - Middleware]({{< ref "/advanced/middleware" >}}).

### Configuration

**Q: Can I use environment variables directly without YAML?**

A: Currently, you need a YAML file, but it can reference environment variables using templates. You can also implement a custom config source.

**Q: How do I handle secrets?**

A: Use environment variables in your YAML templates: `{{env "SECRET_KEY"}}`. Never commit secrets to YAML files.

### Observability

**Q: Can I use Humus without OpenTelemetry?**

A: Yes. Set `otel.sdk.disabled: true` in your configuration.

**Q: How do I send telemetry to multiple backends?**

A: Use an OpenTelemetry Collector to fan out to multiple backends.

**Q: Can I use Prometheus metrics?**

A: Yes. Use the OpenTelemetry Prometheus exporter or collector.

## Still Have Questions?

- Check the [Documentation]({{< ref "/" >}})
- Visit [GitHub Discussions](https://github.com/z5labs/humus/discussions)
- Review [Troubleshooting Guide]({{< ref "troubleshooting" >}})
- See [Best Practices]({{< ref "best-practices" >}})
