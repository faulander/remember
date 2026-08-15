DROP TRIGGER sync_inbox_apply_plan_insert_guard;
DROP TRIGGER sync_inbox_linked_plan_state_guard;
DROP TRIGGER sync_inbox_parent_binding_insert_guard;
DROP VIEW sync_inbox_valid_parent_bindings;
DROP VIEW sync_independent_inbox_candidates;
DROP VIEW sync_unresolved_local_intents;
DROP TRIGGER sync_folder_preserve_delete_clone_insert_guard;
DROP TRIGGER sync_folder_preserve_delete_clone_no_update;
DROP TRIGGER sync_folder_preserve_delete_clone_no_delete;
DROP TRIGGER sync_folder_preserve_delete_note_insert_guard;
DROP TRIGGER sync_folder_preserve_delete_note_no_update;
DROP TRIGGER sync_folder_preserve_delete_note_no_delete;
DROP TRIGGER sync_folder_preserve_delete_resolution_insert_guard;
DROP TRIGGER sync_folder_preserve_delete_resolution_immutable;
DROP TRIGGER sync_folder_preserve_delete_resolution_seal_guard;
DROP TRIGGER sync_folder_preserve_delete_resolution_no_delete;

ALTER TABLE sync_folder_preserve_delete_note_moves RENAME TO sync_folder_preserve_delete_note_moves_v3;
ALTER TABLE sync_folder_preserve_delete_clones RENAME TO sync_folder_preserve_delete_clones_v3;
ALTER TABLE sync_folder_preserve_delete_resolutions RENAME TO sync_folder_preserve_delete_resolutions_v3;

CREATE TABLE sync_folder_preserve_delete_resolutions(
 conflict_operation_id TEXT PRIMARY KEY,
 resolution_operation_id TEXT NOT NULL UNIQUE,
 folder_id TEXT NOT NULL,
 expected_revision INTEGER NOT NULL CHECK(expected_revision>0),
 recovered_folder_id TEXT,
 recovered_cursor INTEGER,
 deleted_cursor INTEGER,
 state TEXT NOT NULL CHECK(state IN('prepared','sealing','resolved')),
 request_version INTEGER NOT NULL CHECK(request_version IN(1,2,3,4)),
 known_cursor INTEGER CHECK(known_cursor IS NULL OR known_cursor>=0),
 first_cursor INTEGER,
 last_cursor INTEGER,
 clone_count INTEGER NOT NULL DEFAULT 0 CHECK(clone_count>=0),
 note_count INTEGER NOT NULL DEFAULT 0 CHECK(note_count>=0),
 recovered_folder_name TEXT CHECK(recovered_folder_name IS NULL OR recovered_folder_name<>''),
 CHECK((state='prepared' AND recovered_folder_id IS NULL AND first_cursor IS NULL AND last_cursor IS NULL) OR (state IN('sealing','resolved') AND recovered_folder_id IS NOT NULL AND recovered_cursor=first_cursor AND deleted_cursor=last_cursor AND first_cursor>0 AND last_cursor>=first_cursor)),
 FOREIGN KEY(conflict_operation_id) REFERENCES sync_outbox(operation_id));
INSERT INTO sync_folder_preserve_delete_resolutions SELECT * FROM sync_folder_preserve_delete_resolutions_v3;

CREATE TABLE sync_folder_preserve_delete_clones(
 conflict_operation_id TEXT NOT NULL,
 ordinal INTEGER NOT NULL CHECK(ordinal>=0),
 original_folder_id TEXT NOT NULL,
 recovered_folder_id TEXT NOT NULL,
 create_cursor INTEGER NOT NULL CHECK(create_cursor>0),
 delete_cursor INTEGER NOT NULL CHECK(delete_cursor>create_cursor),
 local_delete_operation_id TEXT,
 source_revision INTEGER CHECK(source_revision IS NULL OR source_revision>0),
 name TEXT CHECK(name IS NULL OR name<>''),
 source_parent_id TEXT,
 target_parent_id TEXT,
 depth INTEGER CHECK(depth IS NULL OR (depth>0 AND depth<=256)),
 PRIMARY KEY(conflict_operation_id,ordinal),
 UNIQUE(conflict_operation_id,original_folder_id),
 UNIQUE(conflict_operation_id,recovered_folder_id),
 UNIQUE(conflict_operation_id,create_cursor),
 UNIQUE(conflict_operation_id,delete_cursor),
 UNIQUE(conflict_operation_id,local_delete_operation_id),
 FOREIGN KEY(conflict_operation_id) REFERENCES sync_folder_preserve_delete_resolutions(conflict_operation_id),
 FOREIGN KEY(local_delete_operation_id) REFERENCES sync_outbox(operation_id));
INSERT INTO sync_folder_preserve_delete_clones(conflict_operation_id,ordinal,original_folder_id,recovered_folder_id,create_cursor,delete_cursor,local_delete_operation_id,source_revision,name) SELECT conflict_operation_id,ordinal,original_folder_id,recovered_folder_id,create_cursor,delete_cursor,local_delete_operation_id,source_revision,name FROM sync_folder_preserve_delete_clones_v3;

CREATE TABLE sync_folder_preserve_delete_note_moves(
 conflict_operation_id TEXT NOT NULL,
 ordinal INTEGER NOT NULL CHECK(ordinal>=0),
 note_id TEXT NOT NULL,
 move_cursor INTEGER NOT NULL CHECK(move_cursor>0),
 source_revision INTEGER NOT NULL CHECK(source_revision>0),
 target_revision INTEGER NOT NULL CHECK(target_revision=source_revision+1),
 source_parent_id TEXT NOT NULL,
 target_parent_id TEXT NOT NULL,
 name TEXT NOT NULL,
 blob_hash BLOB NOT NULL CHECK(typeof(blob_hash)='blob' AND length(blob_hash)=32),
 local_delete_operation_id TEXT,
 PRIMARY KEY(conflict_operation_id,ordinal),
 UNIQUE(conflict_operation_id,note_id),
 UNIQUE(conflict_operation_id,move_cursor),
 UNIQUE(conflict_operation_id,local_delete_operation_id),
 FOREIGN KEY(conflict_operation_id) REFERENCES sync_folder_preserve_delete_resolutions(conflict_operation_id),
 FOREIGN KEY(local_delete_operation_id) REFERENCES sync_outbox(operation_id));
INSERT INTO sync_folder_preserve_delete_note_moves SELECT * FROM sync_folder_preserve_delete_note_moves_v3;
DROP TABLE sync_folder_preserve_delete_note_moves_v3;
DROP TABLE sync_folder_preserve_delete_clones_v3;
DROP TABLE sync_folder_preserve_delete_resolutions_v3;

CREATE TRIGGER sync_folder_preserve_delete_resolution_insert_guard BEFORE INSERT ON sync_folder_preserve_delete_resolutions
WHEN NEW.state<>'prepared' OR NEW.request_version NOT IN(1,2,3,4) OR (NEW.request_version=1 AND COALESCE(NEW.known_cursor,0)<>0) OR (NEW.request_version IN(2,3,4) AND COALESCE(NEW.known_cursor,0)=0) OR NOT EXISTS(SELECT 1 FROM sync_outbox o JOIN sync_conflict_states c ON c.operation_id=o.operation_id WHERE o.operation_id=NEW.conflict_operation_id AND o.object_id=NEW.folder_id AND o.object_type='folder' AND o.mutation='delete' AND o.status='conflict' AND o.conflict_code='base_revision_mismatch' AND c.object_type='folder' AND c.deleted=0 AND c.revision=NEW.expected_revision)
BEGIN SELECT RAISE(ABORT,'invalid folder preserve delete resolution'); END;
CREATE TRIGGER sync_folder_preserve_delete_resolution_immutable BEFORE UPDATE ON sync_folder_preserve_delete_resolutions
WHEN NOT(
 (OLD.state='prepared' AND NEW.state='prepared' AND OLD.request_version IN(2,3) AND NEW.request_version=4 AND NEW.resolution_operation_id<>OLD.resolution_operation_id AND NEW.conflict_operation_id=OLD.conflict_operation_id AND NEW.folder_id=OLD.folder_id AND NEW.expected_revision=OLD.expected_revision AND NEW.recovered_folder_id IS OLD.recovered_folder_id AND NEW.recovered_folder_name IS OLD.recovered_folder_name AND NEW.recovered_cursor IS OLD.recovered_cursor AND NEW.deleted_cursor IS OLD.deleted_cursor AND NEW.known_cursor IS OLD.known_cursor AND NEW.known_cursor>0 AND NEW.first_cursor IS OLD.first_cursor AND NEW.last_cursor IS OLD.last_cursor AND NEW.clone_count=OLD.clone_count AND NEW.note_count=OLD.note_count)
 OR (OLD.state='prepared' AND NEW.state='sealing' AND NEW.conflict_operation_id=OLD.conflict_operation_id AND NEW.resolution_operation_id=OLD.resolution_operation_id AND NEW.folder_id=OLD.folder_id AND NEW.expected_revision=OLD.expected_revision AND NEW.request_version=OLD.request_version AND NEW.known_cursor IS OLD.known_cursor)
 OR (OLD.state='sealing' AND NEW.state='resolved' AND NEW.conflict_operation_id=OLD.conflict_operation_id AND NEW.resolution_operation_id=OLD.resolution_operation_id AND NEW.folder_id=OLD.folder_id AND NEW.expected_revision=OLD.expected_revision AND NEW.recovered_folder_id=OLD.recovered_folder_id AND NEW.recovered_folder_name IS OLD.recovered_folder_name AND NEW.recovered_cursor=OLD.recovered_cursor AND NEW.deleted_cursor=OLD.deleted_cursor AND NEW.request_version=OLD.request_version AND NEW.known_cursor IS OLD.known_cursor AND NEW.first_cursor=OLD.first_cursor AND NEW.last_cursor=OLD.last_cursor AND NEW.clone_count=OLD.clone_count AND NEW.note_count=OLD.note_count))
BEGIN SELECT RAISE(ABORT,'folder preserve delete resolution immutable'); END;
CREATE TRIGGER sync_folder_preserve_delete_resolution_seal_guard BEFORE UPDATE OF state ON sync_folder_preserve_delete_resolutions
WHEN NOT(
 (OLD.state='prepared' AND NEW.state='sealing' AND NEW.recovered_folder_id IS NOT NULL AND ((NEW.request_version=1 AND NEW.recovered_folder_name IS NULL AND NEW.clone_count=0 AND NEW.note_count=0 AND NEW.last_cursor=NEW.first_cursor+1) OR (NEW.request_version=2 AND NEW.recovered_folder_name IS NULL AND NEW.note_count=0 AND NEW.last_cursor=NEW.first_cursor+2*NEW.clone_count+1) OR (NEW.request_version IN(3,4) AND NEW.recovered_folder_name IS NOT NULL AND NEW.last_cursor=NEW.first_cursor+2*NEW.clone_count+NEW.note_count+1 AND (NEW.request_version<>4 OR 1+NEW.clone_count+NEW.note_count<=10000))))
 OR (OLD.state='sealing' AND NEW.state='resolved' AND (SELECT COUNT(*) FROM sync_folder_preserve_delete_clones c WHERE c.conflict_operation_id=NEW.conflict_operation_id)=NEW.clone_count AND (SELECT COUNT(*) FROM sync_folder_preserve_delete_note_moves n WHERE n.conflict_operation_id=NEW.conflict_operation_id)=NEW.note_count))
BEGIN SELECT RAISE(ABORT,'invalid folder preserve delete resolution transition'); END;
CREATE TRIGGER sync_folder_preserve_delete_resolution_no_delete BEFORE DELETE ON sync_folder_preserve_delete_resolutions BEGIN SELECT RAISE(ABORT,'folder preserve delete resolution durable'); END;

CREATE TRIGGER sync_folder_preserve_delete_clone_insert_guard BEFORE INSERT ON sync_folder_preserve_delete_clones
WHEN NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_resolutions p WHERE p.conflict_operation_id=NEW.conflict_operation_id AND p.state='sealing' AND NEW.ordinal<p.clone_count AND NEW.create_cursor=p.first_cursor+1+NEW.ordinal
 AND ((p.request_version<4 AND NEW.source_parent_id IS NULL AND NEW.target_parent_id IS NULL AND NEW.depth IS NULL AND NEW.delete_cursor=p.first_cursor+1+p.clone_count+(CASE WHEN p.request_version=3 THEN p.note_count ELSE 0 END)+NEW.ordinal AND ((p.request_version=3 AND NEW.source_revision IS NOT NULL AND NEW.name IS NOT NULL) OR (p.request_version<3 AND NEW.source_revision IS NULL AND NEW.name IS NULL)))
 OR (p.request_version=4 AND NEW.source_parent_id IS NOT NULL AND NEW.target_parent_id IS NOT NULL AND NEW.depth BETWEEN 1 AND 256 AND NEW.source_revision IS NOT NULL AND NEW.name IS NOT NULL AND NEW.delete_cursor>=p.first_cursor+1+p.clone_count+p.note_count AND NEW.delete_cursor<p.last_cursor
   AND NEW.original_folder_id<>NEW.recovered_folder_id AND NEW.original_folder_id NOT IN(p.folder_id,p.recovered_folder_id) AND NEW.recovered_folder_id NOT IN(p.folder_id,p.recovered_folder_id)
   AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_clones prior_id WHERE prior_id.conflict_operation_id=p.conflict_operation_id AND (NEW.original_folder_id IN(prior_id.original_folder_id,prior_id.recovered_folder_id) OR NEW.recovered_folder_id IN(prior_id.original_folder_id,prior_id.recovered_folder_id)))
   AND ((NEW.depth=1 AND NEW.source_parent_id=p.folder_id AND NEW.target_parent_id=p.recovered_folder_id) OR EXISTS(SELECT 1 FROM sync_folder_preserve_delete_clones parent WHERE parent.conflict_operation_id=p.conflict_operation_id AND parent.ordinal<NEW.ordinal AND parent.original_folder_id=NEW.source_parent_id AND parent.recovered_folder_id=NEW.target_parent_id AND parent.depth=NEW.depth-1 AND NEW.delete_cursor<parent.delete_cursor))
   AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_clones prior WHERE prior.conflict_operation_id=p.conflict_operation_id AND prior.ordinal<NEW.ordinal AND (prior.depth>NEW.depth OR (prior.depth=NEW.depth AND prior.original_folder_id>=NEW.original_folder_id)))))
 AND (NEW.local_delete_operation_id IS NULL OR EXISTS(WITH RECURSIVE required(operation_id) AS (SELECT dependency_operation_id FROM sync_outbox_dependencies WHERE operation_id=p.conflict_operation_id UNION SELECT d.dependency_operation_id FROM sync_outbox_dependencies d JOIN required r ON d.operation_id=r.operation_id) SELECT 1 FROM sync_outbox o JOIN required r ON r.operation_id=o.operation_id WHERE o.operation_id=NEW.local_delete_operation_id AND o.object_id=NEW.original_folder_id AND o.object_type='folder' AND o.mutation='delete' AND (p.request_version<3 OR o.base_revision=NEW.source_revision))))
BEGIN SELECT RAISE(ABORT,'invalid folder preserve delete clone'); END;
CREATE TRIGGER sync_folder_preserve_delete_note_insert_guard BEFORE INSERT ON sync_folder_preserve_delete_note_moves
WHEN NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_resolutions p WHERE p.conflict_operation_id=NEW.conflict_operation_id AND p.state='sealing' AND p.request_version IN(3,4) AND NEW.ordinal<p.note_count AND NEW.move_cursor=p.first_cursor+1+p.clone_count+NEW.ordinal
 AND ((p.request_version=3 AND NEW.source_parent_id=p.folder_id AND NEW.target_parent_id=p.recovered_folder_id)
 OR (p.request_version=4 AND ((NEW.source_parent_id=p.folder_id AND NEW.target_parent_id=p.recovered_folder_id) OR EXISTS(SELECT 1 FROM sync_folder_preserve_delete_clones parent WHERE parent.conflict_operation_id=p.conflict_operation_id AND parent.original_folder_id=NEW.source_parent_id AND parent.recovered_folder_id=NEW.target_parent_id))
   AND NEW.note_id NOT IN(p.folder_id,p.recovered_folder_id) AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_clones mapped WHERE mapped.conflict_operation_id=p.conflict_operation_id AND NEW.note_id IN(mapped.original_folder_id,mapped.recovered_folder_id))
   AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_note_moves prior WHERE prior.conflict_operation_id=p.conflict_operation_id AND prior.ordinal<NEW.ordinal AND ((CASE WHEN prior.source_parent_id=p.folder_id THEN 0 ELSE 1+(SELECT ordinal FROM sync_folder_preserve_delete_clones c WHERE c.conflict_operation_id=p.conflict_operation_id AND c.original_folder_id=prior.source_parent_id) END)>(CASE WHEN NEW.source_parent_id=p.folder_id THEN 0 ELSE 1+(SELECT ordinal FROM sync_folder_preserve_delete_clones c WHERE c.conflict_operation_id=p.conflict_operation_id AND c.original_folder_id=NEW.source_parent_id) END) OR ((CASE WHEN prior.source_parent_id=p.folder_id THEN 0 ELSE 1+(SELECT ordinal FROM sync_folder_preserve_delete_clones c WHERE c.conflict_operation_id=p.conflict_operation_id AND c.original_folder_id=prior.source_parent_id) END)=(CASE WHEN NEW.source_parent_id=p.folder_id THEN 0 ELSE 1+(SELECT ordinal FROM sync_folder_preserve_delete_clones c WHERE c.conflict_operation_id=p.conflict_operation_id AND c.original_folder_id=NEW.source_parent_id) END) AND prior.note_id>=NEW.note_id)))))
 AND (NEW.local_delete_operation_id IS NULL OR EXISTS(WITH RECURSIVE required(operation_id) AS (SELECT dependency_operation_id FROM sync_outbox_dependencies WHERE operation_id=p.conflict_operation_id UNION SELECT d.dependency_operation_id FROM sync_outbox_dependencies d JOIN required r ON d.operation_id=r.operation_id) SELECT 1 FROM sync_outbox o JOIN required r ON r.operation_id=o.operation_id WHERE o.operation_id=NEW.local_delete_operation_id AND o.object_id=NEW.note_id AND o.object_type='note' AND o.mutation='delete' AND o.base_revision=NEW.source_revision)))
BEGIN SELECT RAISE(ABORT,'invalid folder preserve delete note'); END;
CREATE TRIGGER sync_folder_preserve_delete_clone_no_update BEFORE UPDATE ON sync_folder_preserve_delete_clones BEGIN SELECT RAISE(ABORT,'folder preserve delete clone immutable'); END;
CREATE TRIGGER sync_folder_preserve_delete_clone_no_delete BEFORE DELETE ON sync_folder_preserve_delete_clones BEGIN SELECT RAISE(ABORT,'folder preserve delete clone durable'); END;
CREATE TRIGGER sync_folder_preserve_delete_note_no_update BEFORE UPDATE ON sync_folder_preserve_delete_note_moves BEGIN SELECT RAISE(ABORT,'folder preserve delete note immutable'); END;
CREATE TRIGGER sync_folder_preserve_delete_note_no_delete BEFORE DELETE ON sync_folder_preserve_delete_note_moves BEGIN SELECT RAISE(ABORT,'folder preserve delete note durable'); END;

CREATE VIEW sync_unresolved_local_intents AS SELECT DISTINCT o.object_id FROM sync_outbox o WHERE
 (o.status IN('pending','attempted','replay_mismatch') AND NOT EXISTS(SELECT 1 FROM conflict_folder_create_note_members n JOIN conflict_folder_create_recoveries f ON f.operation_id=n.operation_id WHERE n.old_operation_id=o.operation_id AND f.state IN('moved','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_note_members n JOIN conflict_folder_move_delete_recoveries d ON d.operation_id=n.operation_id WHERE n.old_operation_id=o.operation_id AND d.state IN('moved','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_note_members n JOIN conflict_folder_divergent_move_recoveries d ON d.operation_id=n.operation_id WHERE (n.old_operation_id=o.operation_id OR EXISTS(SELECT 1 FROM conflict_folder_divergent_move_note_chains c WHERE c.operation_id=n.operation_id AND c.note_id=n.note_id AND c.old_operation_id=o.operation_id)) AND d.state IN('evacuated','canonical_published','completed')) AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_clones c JOIN sync_folder_preserve_delete_resolutions p ON p.conflict_operation_id=c.conflict_operation_id WHERE c.local_delete_operation_id=o.operation_id AND p.state='resolved') AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_note_moves n JOIN sync_folder_preserve_delete_resolutions p ON p.conflict_operation_id=n.conflict_operation_id WHERE n.local_delete_operation_id=o.operation_id AND p.state='resolved'))
 OR (o.status='conflict' AND NOT EXISTS(SELECT 1 FROM sync_conflict_resolutions r WHERE r.operation_id=o.operation_id) AND NOT EXISTS(SELECT 1 FROM conflict_materializations m WHERE m.operation_id=o.operation_id AND m.state IN('copy_staged','copy_published','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_create_recoveries f WHERE f.operation_id=o.operation_id AND f.state IN('moved','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_move_delete_recoveries d WHERE d.operation_id=o.operation_id AND d.state IN('moved','completed')) AND NOT EXISTS(SELECT 1 FROM conflict_folder_divergent_move_recoveries d WHERE d.operation_id=o.operation_id AND d.state IN('evacuated','canonical_published','completed')) AND NOT EXISTS(SELECT 1 FROM sync_folder_preserve_delete_resolutions p WHERE p.conflict_operation_id=o.operation_id AND p.state='resolved'));

CREATE VIEW sync_independent_inbox_candidates AS SELECT i.* FROM sync_inbox_changes i WHERE i.state='pending' AND i.object_type='note' AND i.mutation IN('update','delete') AND EXISTS(SELECT 1 FROM sync_baselines baseline WHERE baseline.object_id=i.object_id AND baseline.revision=i.revision-1) AND NOT EXISTS(SELECT 1 FROM sync_inbox_changes earlier WHERE earlier.object_id=i.object_id AND earlier.cursor<i.cursor AND earlier.state<>'applied') AND NOT EXISTS(SELECT 1 FROM sync_unresolved_local_intents unresolved WHERE unresolved.object_id=i.object_id) AND (i.parent_id IS NULL OR EXISTS(SELECT 1 FROM objects parent JOIN sync_baselines parent_baseline ON parent_baseline.object_id=parent.object_id WHERE parent.object_id=i.parent_id AND parent.object_type='folder' AND parent.parent_id IS NULL AND parent.identity_state='known' AND parent.folder_device>0 AND parent.folder_inode>0 AND parent_baseline.revision>0 AND parent_baseline.operation_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM sync_inbox_changes parent_earlier WHERE parent_earlier.object_id=parent.object_id AND parent_earlier.cursor<i.cursor AND parent_earlier.state<>'applied') AND NOT EXISTS(SELECT 1 FROM sync_unresolved_local_intents parent_unresolved WHERE parent_unresolved.object_id=parent.object_id)));
CREATE VIEW sync_inbox_valid_parent_bindings AS SELECT binding.* FROM sync_inbox_parent_bindings binding JOIN sync_inbox_changes i ON i.cursor=binding.inbox_cursor JOIN objects parent ON parent.object_id=binding.parent_id JOIN sync_baselines baseline ON baseline.object_id=binding.parent_id WHERE i.parent_id=binding.parent_id AND i.state IN('pending','applying','applied') AND parent.object_type='folder' AND parent.parent_id IS NULL AND parent.identity_state='known' AND parent.relative_path=binding.parent_relative AND parent.folder_device=binding.device AND parent.folder_inode=binding.inode AND baseline.revision=binding.baseline_revision AND baseline.operation_id=binding.baseline_operation_id AND NOT EXISTS(SELECT 1 FROM sync_inbox_changes parent_earlier WHERE parent_earlier.object_id=binding.parent_id AND parent_earlier.cursor<binding.inbox_cursor AND parent_earlier.state<>'applied') AND NOT EXISTS(SELECT 1 FROM sync_unresolved_local_intents parent_unresolved WHERE parent_unresolved.object_id=binding.parent_id);
CREATE TRIGGER sync_inbox_parent_binding_insert_guard BEFORE INSERT ON sync_inbox_parent_bindings WHEN NOT EXISTS(SELECT 1 FROM apply_plans p JOIN sync_inbox_changes i ON i.cursor=NEW.inbox_cursor JOIN objects parent ON parent.object_id=i.parent_id JOIN sync_baselines baseline ON baseline.object_id=parent.object_id WHERE p.plan_id=NEW.plan_id AND p.status='prepared' AND i.state='pending' AND EXISTS(SELECT 1 FROM sync_independent_inbox_candidates eligible WHERE eligible.cursor=i.cursor) AND NEW.parent_id=i.parent_id AND NEW.parent_relative=parent.relative_path AND NEW.device=parent.folder_device AND NEW.inode=parent.folder_inode AND NEW.baseline_revision=baseline.revision AND NEW.baseline_operation_id=baseline.operation_id) BEGIN SELECT RAISE(ABORT,'invalid sync inbox parent binding'); END;
CREATE TRIGGER sync_inbox_apply_plan_insert_guard BEFORE INSERT ON sync_inbox_apply_plans WHEN NOT EXISTS(SELECT 1 FROM apply_plans p JOIN apply_steps s ON s.plan_id=p.plan_id AND s.step_index=0 JOIN sync_inbox_changes i ON i.cursor=NEW.cursor WHERE p.plan_id=NEW.plan_id AND p.status='prepared' AND p.from_cursor=NEW.cursor-1 AND p.through_cursor=NEW.cursor AND (SELECT count(*) FROM apply_steps x WHERE x.plan_id=p.plan_id)=1 AND s.cursor=i.cursor AND s.operation_id=i.operation_id AND s.object_id=i.object_id AND s.mutation=i.mutation AND s.object_type=i.object_type AND s.revision=i.revision AND s.parent_id IS i.parent_id AND s.name=i.name AND s.blob_hash IS i.blob_hash AND s.state='pending' AND EXISTS(SELECT 1 FROM sync_independent_inbox_candidates eligible WHERE eligible.cursor=i.cursor) AND ((i.parent_id IS NULL AND NOT EXISTS(SELECT 1 FROM sync_inbox_parent_bindings binding WHERE binding.plan_id=p.plan_id)) OR (i.parent_id IS NOT NULL AND EXISTS(SELECT 1 FROM sync_inbox_valid_parent_bindings binding WHERE binding.plan_id=p.plan_id AND binding.inbox_cursor=i.cursor))) AND (SELECT count(*) FROM apply_plans active WHERE active.status IN('prepared','applying'))=1) BEGIN SELECT RAISE(ABORT,'invalid sync inbox apply plan link'); END;
CREATE TRIGGER sync_inbox_linked_plan_state_guard BEFORE UPDATE OF status,completed_at_ms ON apply_plans WHEN EXISTS(SELECT 1 FROM sync_inbox_apply_plans l WHERE l.plan_id=OLD.plan_id) AND NOT((OLD.status='prepared' AND NEW.status='applying' AND NEW.completed_at_ms IS NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='applying' AND s.state='pending' AND ((i.parent_id IS NULL AND NOT EXISTS(SELECT 1 FROM sync_inbox_parent_bindings binding WHERE binding.plan_id=OLD.plan_id)) OR EXISTS(SELECT 1 FROM sync_inbox_valid_parent_bindings binding WHERE binding.plan_id=OLD.plan_id AND binding.inbox_cursor=i.cursor)))) OR (OLD.status='prepared' AND NEW.status='failed' AND NEW.completed_at_ms IS NOT NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='pending' AND s.state='pending' AND NOT EXISTS(SELECT 1 FROM apply_folder_publications f WHERE f.plan_id=OLD.plan_id) AND NOT EXISTS(SELECT 1 FROM apply_folder_mutations m WHERE m.plan_id=OLD.plan_id))) OR (OLD.status='failed' AND NEW.status='prepared' AND NEW.completed_at_ms IS NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='pending' AND s.state='pending' AND NOT EXISTS(SELECT 1 FROM apply_folder_publications f WHERE f.plan_id=OLD.plan_id) AND NOT EXISTS(SELECT 1 FROM apply_folder_mutations m WHERE m.plan_id=OLD.plan_id) AND EXISTS(SELECT 1 FROM sync_independent_inbox_candidates eligible WHERE eligible.cursor=i.cursor) AND ((i.parent_id IS NULL AND NOT EXISTS(SELECT 1 FROM sync_inbox_parent_bindings binding WHERE binding.plan_id=OLD.plan_id)) OR EXISTS(SELECT 1 FROM sync_inbox_valid_parent_bindings binding WHERE binding.plan_id=OLD.plan_id AND binding.inbox_cursor=i.cursor)) AND NOT EXISTS(SELECT 1 FROM apply_plans active WHERE active.plan_id<>OLD.plan_id AND active.status IN('prepared','applying')))) OR (OLD.status='applying' AND NEW.status='completed' AND NEW.completed_at_ms IS NOT NULL AND EXISTS(SELECT 1 FROM sync_inbox_apply_plans l JOIN sync_inbox_changes i ON i.cursor=l.cursor JOIN apply_steps s ON s.plan_id=OLD.plan_id AND s.step_index=0 WHERE l.plan_id=OLD.plan_id AND i.state='applied' AND s.state='applied' AND EXISTS(SELECT 1 FROM sync_baselines baseline WHERE baseline.object_id=i.object_id AND baseline.revision=i.revision AND baseline.operation_id=i.operation_id) AND ((i.parent_id IS NULL AND NOT EXISTS(SELECT 1 FROM sync_inbox_parent_bindings binding WHERE binding.plan_id=OLD.plan_id)) OR EXISTS(SELECT 1 FROM sync_inbox_valid_parent_bindings binding WHERE binding.plan_id=OLD.plan_id AND binding.inbox_cursor=i.cursor))))) BEGIN SELECT RAISE(ABORT,'linked inbox apply plan state transition is inconsistent'); END;
