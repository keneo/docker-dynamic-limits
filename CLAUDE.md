# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**docker-dynamic-limits** is a system for managing dynamic resource limits on Docker containers. It provides per-container control over:

- CPU total usage (with container freezing when limits are reached)
- External paid services spending budget (LLMs, etc.)
- Disk space usage
- RAM usage
- Network total usage (bytes)
- Disk I/O total bytes
- Disk I/O total operations

## Key Requirements

- For each container and each limit type: check current usage, set/increase/decrease limits
- Container cloning capability
- Containers must be able to query their own limits and usage from inside the container
