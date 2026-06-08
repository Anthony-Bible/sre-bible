# Technology Stack

## Core Language & Runtime
- **Go 1.26:** Primary backend programming language.

## Database & Storage
- **PostgreSQL:** Primary relational database.
- **pgx/v5:** PostgreSQL driver and toolkit for Go.
- **pgvector (`pgvector-go`):** Vector storage for semantic search and AI embedding matching.
- **Goose (`pressly/goose`):** Database schema migration management.

## AI & Machine Learning
- **Anthropic SDK (`anthropic-sdk-go`):** Integration with Anthropic's Claude models.
- **Google GenAI (`google.golang.org/genai`):** Integration with Google's Gemini models.

## Cloud & Infrastructure
- **GKE (Google Kubernetes Engine):** Primary cloud deployment and container orchestration platform.
- **AWS SES (`aws-sdk-go-v2/service/sesv2`):** Used exclusively for sending transactional emails (Simple Email Service).