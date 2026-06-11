# Session Context

## User Prompts

### Prompt 1

Help me fix the issues reported by /doctor below.

For each issue: briefly explain what the fix will do, then ask me to confirm before running any shell command that deletes files, modifies global config, or changes my installation. Safe read-only checks are fine without asking. If a suggested fix looks wrong for my setup, say so instead of running it.

- Settings (/Users/jobinlawrance/.claude/settings.json › permissions.allow): Invalid permission rule "mcp__*" was skipped: Wildcard tool name ...

### Prompt 2

Base directory for this skill: /Users/jobinlawrance/.claude/skills/Interview

# Interview — phased conversational context review + fill

## 🚨 MANDATORY: Voice Notification

Before running the workflow, send:

```bash
curl -s -X POST http://localhost:31337/notify \
  -H "Content-Type: application/json" \
  -d '{"message": "Starting the interview. Scanning phases first."}' \
  > /dev/null 2>&1 &
```

## What this skill does

Runs a **phased conversational interview** across every PAI context ...

### Prompt 3

I just want to create software that is free and useful for other people, and I just want to have a good time. Hopefully, bad things in the world stop happening, and if I can contribute in some way, I will.

### Prompt 4

install curl -fsSL https://raw.githubusercontent.com/JuliusBrussee/caveman/main/install.sh  in pai

### Prompt 5

Base directory for this skill: /Users/jobinlawrance/Project/caw/.claude/skills/caveman

Respond terse like smart caveman. All technical substance stay. Only fluff die.

## Persistence

ACTIVE EVERY RESPONSE. No revert after many turns. No filler drift. Still active if unsure. Off only: "stop caveman" / "normal mode".

Default: **full**. Switch: `/caveman lite|full|ultra`.

## Rules

Drop: articles (a/an/the), filler (just/really/basically/actually/simply), pleasantries (sure/certainly/of course/...

### Prompt 6

Base directory for this skill: /Users/jobinlawrance/Project/caw/.claude/skills/caveman

Respond terse like smart caveman. All technical substance stay. Only fluff die.

## Persistence

ACTIVE EVERY RESPONSE. No revert after many turns. No filler drift. Still active if unsure. Off only: "stop caveman" / "normal mode".

Default: **full**. Switch: `/caveman lite|full|ultra`.

## Rules

Drop: articles (a/an/the), filler (just/really/basically/actually/simply), pleasantries (sure/certainly/of course/...

### Prompt 7

Personal mostly

### Prompt 8

Ship Raven, caw, Ravencloak, viewrr etc

### Prompt 9

well after a agent tries to create a PR it has to constantly keep pinging the gh cli to check if PR passed or if any action failed. caw is a plugin/mcp that the agent can subscribe to after raising a PR via a SSE which instanly updates the agents to the whereabouts of the PR without polling. This is what caw is set to fix

### Prompt 10

skip for now, they are separate projects

### Prompt 11

solo, dogfooding, build in public

### Prompt 12

perfectionism, procrastination.

### Prompt 13

indie dev who has crazy ideas and too little time to finish/

### Prompt 14

Well equality for all, simple beats clever, free tech

### Prompt 15

Trust none, stupidity > mallice

### Prompt 16

music, movies, cooking

### Prompt 17

wrap Phase 1

### Prompt 18

phase 2, but why are we doing all this ?

### Prompt 19

healthy

### Prompt 20

freedom from job, fuck you money

### Prompt 21

all of the above

### Prompt 22

skip

### Prompt 23

cook more, make mead

### Prompt 24

stop here

### Prompt 25

what's left to setup PAI

### Prompt 26

yes

### Prompt 27

location - BENGALURU, role - engineering lead, focus - finish car and deploy

### Prompt 28

1. Name

### Prompt 29

1. Name - Alfred, Personality - pushes back, opinionated

### Prompt 30

let's do what's left

### Prompt 31

Raven - AI knowledge base with widgets, chatbot, webRTC calling feature, Ravencloak - A custom idp for keycloak, viewrr - TBD

### Prompt 32

he/him. 2- https://github.com/jobinlawrance/ 3. IST daylight,  4 - pomodoro

### Prompt 33

10 years exp, core skills - kotlin etc. take resume from ~/Downloads/JobinLawranceResume.pdf

### Prompt 34

yes

### Prompt 35

anything else left to setup PAI, how do I use PAI the best?

### Prompt 36

Base directory for this skill: /Users/jobinlawrance/.claude/skills/graphify

# /graphify

Turn any folder of files into a navigable knowledge graph with community detection, an honest audit trail, and three outputs: interactive HTML, GraphRAG-ready JSON, and a plain-language GRAPH_REPORT.md.

## Usage

```
/graphify                                             # full pipeline on current directory → Obsidian vault
/graphify <path>                                      # full pipeline on specific ...

### Prompt 37

Build graph on all of caw

### Prompt 38

what's the project status using graphify

