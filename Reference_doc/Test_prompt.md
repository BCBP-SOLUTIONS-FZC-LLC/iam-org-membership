# Universal Go Microservice API Automation Testing Prompt

## Purpose

This prompt instructs Claude to design, generate, review, and maintain comprehensive automated tests for Go-based microservices, with a strong focus on enterprise backend APIs. It should drive maximum practical code coverage across positive, negative, boundary, security, authorization, database, event, concurrency, and performance scenarios; enforce API contract validation; maintain a `Reference_doc/test_cover.md` progress file; and resume seamlessly from prior work in future sessions.

This document defines the standard testing methodology for all backend services developed in this project.

It is applicable to every current and future service, including but not limited to:

- User Profile Service
- Org & Membership Service
- Workflow Service
- Notification Service
- Tender Service
- Billing Service
- IAM Services
- Any future microservice

This document serves as the master instruction for generating production-quality API automation test cases, maintaining testing progress, and ensuring complete implementation coverage.

---

# Primary Objective

Generate a comprehensive automation test suite that achieves the highest possible quality and code coverage.

### Coverage Goals

Every API should aim to achieve:

- 100% Statement Coverage
- 100% Branch Coverage
- 100% Condition Coverage
- 100% Exception Coverage
- 100% Business Rule Coverage
- 100% Authorization Coverage
- 100% API Contract Coverage
- Maximum Mutation Coverage

Never stop after generating only happy-path scenarios.

Every possible execution path, validation, exception, business rule, workflow, and lifecycle must be covered.

---
## Documentation Naming Rule

All generated documentation files must have meaningful, self-explanatory filenames that accurately reflect their contents. Never use generic or misleading names; ensure every file name matches its purpose and follows a consistent project naming convention.

# Documents to Analyze

Before generating test cases, always analyze:

- High Level Design (HLD)
- Low Level Design (LLD)
- OpenAPI Specification
- AsyncAPI Specification (if available)
- API Documentation
- Database Schema
- Event Flow Documentation
- Business Rules
- Authorization Rules
- Validation Rules
- Sequence Diagrams (if available)

Do not assume business rules.

Derive every test case from the provided documentation.

---

# LLD Compliance Rule (Highest Priority)

The Low Level Design (LLD) is the primary source of truth for all test generation.

Before generating any test cases, thoroughly analyze the LLD and ensure every generated test case aligns with the documented implementation.

Do not assume business logic, workflows, validations, or system behavior that are not defined in the LLD or supporting documents.

Every generated test case must be traceable to one or more requirements defined in the LLD.

---

## Mandatory LLD Validation

For every API, feature, module, or workflow, validate everything documented in the LLD, including but not limited to:

- Business Rules
- Functional Requirements
- Request Validation
- Response Validation
- HTTP Status Codes
- Business Error Codes
- Error Messages
- Authorization Rules
- Authentication Rules
- Validation Rules
- Database Operations
- Transactions
- Rollback Behavior
- Soft Deletes
- Audit Fields
- Cache Updates
- Event Generation
- Event Consumption
- AsyncAPI Definitions (if available)
- SNS Topics
- SQS Queues
- Event Payloads
- Event Metadata
- Retry Logic
- Idempotency
- Background Jobs
- Scheduled Jobs (Cron)
- State Transitions
- Entity Lifecycles
- Configuration-Based Behavior
- Feature Flags
- Cross-Service Communication
- Failure Handling
- Recovery Scenarios
- Concurrency Handling
- Performance Requirements

Every documented requirement in the LLD must have corresponding test coverage.

---

## Complete Scenario Coverage

Do not stop after generating only Happy Path scenarios.

Every documented or implied scenario in the LLD must be covered.

Generate test cases for:

- Positive Scenarios
- Negative Scenarios
- Boundary Scenarios
- Validation Scenarios
- Business Rule Scenarios
- Authorization Scenarios
- Authentication Scenarios
- Database Validation Scenarios
- Event Validation Scenarios
- Lifecycle Scenarios
- State Transition Scenarios
- Retry Scenarios
- Rollback Scenarios
- Failure Scenarios
- Recovery Scenarios
- Concurrency Scenarios
- Performance Scenarios
- API Contract Validation Scenarios

Every documented:

- API
- Workflow
- Business Rule
- Status Code
- Error Code
- Database Operation
- Event
- Event Flow
- Lifecycle
- State Transition
- Cron Job
- Background Worker
- Cross-Service Interaction

must have one or more corresponding test cases.

The objective is to achieve complete implementation coverage based on the LLD—not just REST API coverage.

---

# Mandatory Test Categories

## 1. Positive Testing

Generate scenarios for:

- Happy Path
- Valid Inputs
- Multiple Valid Combinations
- Different User Roles
- Different Tenant States
- Different Resource States
- Different Configuration States

---

## 2. Negative Testing

Generate scenarios for:

- Missing Required Fields
- Invalid UUID
- Invalid Enum
- Invalid Data Types
- Invalid JSON
- Invalid Formats
- Null Values
- Empty Values
- Duplicate Requests
- Duplicate Resources
- Unauthorized Requests (401)
- Forbidden Requests (403)
- Resource Not Found (404)
- Conflict (409)
- Validation Failures (422)
- Internal Server Errors (500)

---

## 3. Boundary Testing

Generate scenarios for:

- Minimum Values
- Maximum Values
- Maximum String Length
- Minimum String Length
- Unicode Characters
- Special Characters
- Large Payloads
- Empty Collections
- Maximum Collections

---

## 4. Business Rule Testing

Generate test cases for every business rule defined in the LLD and HLD.

Include:

- Success Scenarios
- Failure Scenarios
- Validation Rules
- Workflow Rules
- Authorization Rules
- Lifecycle Rules
- State Transitions

---

## 5. Branch Coverage

Generate scenarios covering every branch, including:

- if
- else
- switch
- default
- continue
- break
- retry
- rollback
- exception
- early return

---

## 6. Database Validation

Validate:

- Inserts
- Updates
- Deletes
- Soft Deletes
- Audit Columns
- Foreign Keys
- Constraints
- Transactions
- Rollback
- Outbox Table
- processed_events Table
- Cache Updates

---

## 7. Event Validation

Whenever events are involved, validate:

- Outbox Event
- Event Payload
- Event Schema
- Event Type
- Event Version
- SNS Topic
- SQS Queue
- Event Metadata
- Correlation IDs
- Event Publishing
- Event Consumption
- Consumer Processing
- Retry Behaviour
- Dead Letter Queue Handling
- Idempotency
- Eventual Consistency

---

## 8. Authorization Testing

Generate scenarios for every supported role defined in the LLD.

---

## 9. Security Testing

Generate scenarios for:

- JWT Validation
- Missing Token
- Expired Token
- Invalid Token
- SQL Injection
- XSS
- Replay Attack
- Parameter Tampering
- Privilege Escalation
- Cross-Tenant Access

---

## 10. Concurrency Testing

Generate scenarios for:

- Concurrent Requests
- Duplicate Requests
- Retry Logic
- Race Conditions
- Locking
- Idempotency

---

## 11. Performance Testing

Generate scenarios for:

- Large Payloads
- Load Testing
- Stress Testing
- Response Time
- Bulk Operations

---

## 12. API Contract Validation

Validate:

- Request Schema
- Response Schema
- HTTP Status Codes
- Headers
- Error Responses
- Pagination
- Filtering
- Sorting

---

## 13. Assertions

Every automation test should validate:

- HTTP Status
- Response Body
- Response Headers
- Database Changes
- Event Generation
- Cache Updates
- Audit Logs
- Side Effects

---

## 14. Test Data

Generate:

- Valid Data
- Invalid Data
- Boundary Data
- Duplicate Data
- Expired Data
- Deleted Resources
- Cross-Tenant Data

---

## 15. Test Implementation

Generate production-ready automation code only when explicitly requested.

The implementation must:

- Follow the project's existing testing framework.
- Avoid duplicate code.
- Be reusable and maintainable.
- Follow the project's coding standards.

---

# Test Case Documentation Format

Every generated test case should include:

- Test Case ID
- Module
- Feature
- API
- Scenario Name
- Preconditions
- Test Steps
- Expected Result
- Priority
- Severity
- Automation Status

---

# Progress Tracking (Mandatory)

Before starting any testing work, create the following file if it does not already exist:

```
Reference_doc/test_cover.md
```

This file is mandatory.

Its purpose is to maintain testing progress across multiple AI sessions.

---

# Rules for test_cover.md

Update `Reference_doc/test_cover.md` **only after** the user-requested testing task has been fully completed.

Do NOT update:

- During intermediate progress
- After every response
- While work is incomplete

Update only after completing:

- API testing
- Feature testing
- Module testing
- Phase testing
- Regression testing
- User-requested testing tasks

---

# test_cover.md Contents

Maintain:

## Service Information

- Service Name
- Repository Name

## Current Progress

- Current Phase
- Current Module
- Current Feature

## Completed Tasks

Include:

- Task Name
- APIs Covered
- Features Covered
- Business Rules Covered
- Database Tables Covered
- Event Flows Covered
- Authorization Scenarios Covered
- Test Categories Covered

## Pending Work

Maintain:

- Pending APIs
- Pending Features
- Pending Modules
- Pending Phases
- Missing Scenarios

## Coverage Summary

Track:

- Positive Testing
- Negative Testing
- Boundary Testing
- Business Rules
- Security
- Authorization
- Database Validation
- Event Validation
- Concurrency
- Performance
- API Contract
- Test Implementation

## Continuation

Maintain:

- Last Completed User Task
- Next Recommended Task

Future AI sessions must first read this file and continue from the last recorded point.

---

# Excel Documentation Policy

The Excel workbook is the official test case document.

Never update the Excel workbook automatically.

Only update the Excel workbook when the user explicitly requests it.

If the user has not requested an Excel update, continue generating test cases and maintain only `Reference_doc/test_cover.md`.

---

# Excel Path Confirmation

Before updating Excel, always ask:

> Please provide the complete path of the Excel workbook that should be updated.

Never assume the workbook path.

---

# Excel Validation Checklist

Before updating Excel verify:

- Testing task is complete.
- Test cases are reviewed.
- Duplicate cases removed.
- Test Case IDs are unique.
- Formatting is consistent.
- Excel path confirmed.

Only then update the workbook.

---

# What to Write into Excel

Write only finalized test cases.

Do NOT write:

- Draft cases
- Incomplete scenarios
- Placeholder content
- Duplicate cases
- Partially reviewed cases

---

# General Rules

Always:

- Read the HLD.
- Read the LLD.
- Read the OpenAPI Specification.
- Read the AsyncAPI Specification (if available).
- Read the API Documentation.
- Read the Database Schema.
- Read the Event Documentation.
- Treat the LLD as the primary source of truth.
- Generate test cases from documented business rules.
- Validate every documented HTTP status code.
- Validate every documented business error code.
- Validate every documented database operation.
- Validate every documented event flow.
- Validate every documented workflow.
- Validate every documented lifecycle.
- Validate every documented state transition.
- Validate every documented authorization rule.
- Validate every documented business rule.
- Ensure every documented LLD requirement has corresponding test coverage.
- Think like a Senior SDET reviewing a production-grade enterprise backend system.

Never:

- Skip edge cases.
- Skip failure scenarios.
- Assume undocumented business rules.
- Generate duplicate test cases.
- Ignore LLD requirements.
- Lose testing progress across sessions.
- Update the Excel workbook without explicit user instruction.
- Assume the Excel workbook location.

The objective is to produce a reusable, maintainable, production-ready automation test suite that validates the complete implementation exactly as defined in the LLD while maintaining testing progress through `Reference_doc/test_cover.md` and updating the official Excel workbook only when explicitly requested by the user.