---
title: "[0001] Use MADR for Architecture Decision Records"
description: >
    Adopt Markdown Architectural Decision Records (MADR) as the standard format for documenting architectural decisions in the project.
type: docs
weight: 1
category: "strategic"
status: "accepted"
date: 2025-12-14
deciders: []
consulted: []
informed: []
---

## Context and Problem Statement

As the project grows, architectural decisions are made that have long-term impacts on the system's design, maintainability, and scalability. Without a structured way to document these decisions, we risk losing the context and rationale behind important choices, making it difficult for current and future team members to understand why certain approaches were taken.

How should we document architectural decisions in a way that is accessible, maintainable, and provides sufficient context for future reference?

## Decision Drivers

* Need for clear documentation of architectural decisions and their rationale
* Easy accessibility and searchability of past decisions
* Low barrier to entry for creating and maintaining decision records
* Integration with existing documentation workflow
* Version control friendly format
* Industry-standard approach that team members may already be familiar with

## Considered Options

* MADR (Markdown Architectural Decision Records)
* ADR using custom format
* Wiki-based documentation
* No formal ADR process

## Decision Outcome

Chosen option: "MADR (Markdown Architectural Decision Records)", because it provides a well-established, standardized format that is lightweight, version-controlled, and integrates seamlessly with our existing documentation structure. MADR 4.0.0 offers a clear template that captures all necessary information while remaining flexible enough for different types of decisions.

### Consequences

* Good, because MADR is a widely adopted standard with clear documentation and examples
* Good, because markdown files are easy to create, edit, and review through pull requests
* Good, because ADRs will be version-controlled alongside code, maintaining historical context
* Good, because the format is flexible enough to accommodate strategic, user-journey, and API design decisions
* Good, because team members can easily search and reference past decisions
* Neutral, because requires discipline to maintain and update ADR status as decisions evolve
* Bad, because team members need to learn and follow the MADR format conventions

### Confirmation

Compliance will be confirmed through:
* Code reviews ensuring new architectural decisions are documented as ADRs
* ADRs are stored in `docs/content/technical_documentation/adrs/` following the naming convention `NNNN-title-with-dashes.md`
* Regular reviews during architecture discussions to reference and update existing ADRs

## Pros and Cons of the Options

### MADR (Markdown Architectural Decision Records)

MADR 4.0.0 is a standardized format for documenting architectural decisions using markdown.

* Good, because it's a well-established standard with extensive documentation
* Good, because markdown is simple, portable, and version-control friendly
* Good, because it provides a clear structure while remaining flexible
* Good, because it integrates with static site generators and documentation tools
* Good, because it's lightweight and doesn't require special tools
* Neutral, because it requires some initial learning of the format
* Neutral, because maintaining consistency requires discipline

### ADR using custom format

Create our own custom format for architectural decision records.

* Good, because we can tailor it exactly to our needs
* Bad, because it requires defining and maintaining our own standard
* Bad, because new team members won't be familiar with the format
* Bad, because we lose the benefits of community knowledge and tooling
* Bad, because it may evolve inconsistently over time

### Wiki-based documentation

Use a wiki system (like Confluence, Notion, or GitHub Wiki) to document decisions.

* Good, because wikis provide easy editing and hyperlinking
* Good, because some team members may be familiar with wiki tools
* Neutral, because it may or may not integrate with version control
* Bad, because content may not be version-controlled alongside code
* Bad, because it creates a separate system to maintain
* Bad, because it's harder to review changes through standard PR process
* Bad, because portability and long-term accessibility may be concerns

### No formal ADR process

Continue without a structured approach to documenting architectural decisions.

* Good, because it requires no additional overhead
* Bad, because context and rationale for decisions are lost over time
* Bad, because new team members struggle to understand why decisions were made
* Bad, because it leads to repeated discussions of previously settled questions
* Bad, because it makes it difficult to track when decisions should be revisited

<!-- This is an optional element. Feel free to remove. -->
## More Information

* MADR 4.0.0 specification: https://adr.github.io/madr/
* ADRs will be categorized as: strategic, user-journey, or api-design
* ADR status values: proposed | accepted | rejected | deprecated | superseded by ADR-XXXX
* All ADRs are stored in `docs/content/technical_documentation/adrs/` directory