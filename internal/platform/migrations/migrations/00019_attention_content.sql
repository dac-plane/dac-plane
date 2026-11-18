-- +goose Up
-- 00019: the text of a question and of its answer (ADR-0026, #157).
--
-- Additive: version 18 keeps serving. An attention request still carries no text
-- of its own; a question's prompts sit in attention_question and an answer's text
-- in decision_answer (keyed by the decision, which names its request), both written once with the record they belong to and never
-- changed. The decision keeps only the answer's digest. Neither table holds a
-- secret: the adapter declines a secret prompt before any text reaches the product.

CREATE TABLE attention_question (
  tenant_id    uuid        NOT NULL REFERENCES tenant (id),
  attention_id uuid        NOT NULL,
  content      jsonb       NOT NULL CHECK (jsonb_typeof(content) = 'object' AND octet_length(content::text) <= 65536),
  created_at   timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, attention_id),
  FOREIGN KEY (tenant_id, attention_id) REFERENCES attention_request (tenant_id, attention_id)
);

CREATE TABLE decision_answer (
  tenant_id    uuid        NOT NULL REFERENCES tenant (id),
  decision_id  uuid        NOT NULL,
  content      jsonb       NOT NULL CHECK (jsonb_typeof(content) = 'object' AND octet_length(content::text) <= 65536),
  created_at   timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, decision_id),
  FOREIGN KEY (tenant_id, decision_id) REFERENCES decision (tenant_id, decision_id)
);

CREATE TRIGGER attention_question_immutable
  BEFORE UPDATE OR DELETE ON attention_question
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER decision_answer_immutable
  BEFORE UPDATE OR DELETE ON decision_answer
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- +goose Down
DROP TABLE decision_answer;
DROP TABLE attention_question;
