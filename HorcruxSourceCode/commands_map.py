import json
import os
from commands import *
import random

# self.map: the table
# self.temp_root_command_list: list of Command
# self.temp_parallel_command_list: list of Command
class CommandMap:

    def __init__(self):
        self.create_new_map()

    def create_new_map(self):
        self.temp_root_command_list = []
        self.temp_parallel_command_list = []

        commands = get_commands()
        # root_command_list
        for command in commands:
            opcode = command['name']
            param = []
            # single param
            if command['argument_num'] == 1:
                param.append(Relation.SELF)
            # double param
            else:
                param.append(Relation.SELF)
                param.append(Relation.SELF)
            # three ano
            for ano in list(Ano):
                root_com = Command(opcode, param, ano)
                self.temp_root_command_list.append(root_com)

        # parallel_command_list
        for command in commands:
            opcode = command['name']
            # single param
            if command['argument_num'] == 1:
                for relation in list(Relation):
                    # the relation of the first param cannot be NREL
                    if relation == Relation.NREL:
                        continue
                    param = []
                    param.append(relation)
                    for ano in list(Ano):
                        paral_com = Command(opcode, param, ano)
                        self.temp_parallel_command_list.append(paral_com)
            # double param
            else:
                for relation1 in list(Relation):
                    # the relation of the first param cannot be NREL
                    if relation1 == Relation.NREL:
                        continue
                    for relation2 in list(Relation):
                        param = []
                        param.append(relation1)
                        param.append(relation2)
                        for ano in list(Ano):
                            paral_com = Command(opcode, param, ano)
                            self.temp_parallel_command_list.append(paral_com)
            self.map = {}
            for root_com in self.temp_root_command_list:
                self.map[str(root_com)] = {}
                for paral_com in self.temp_parallel_command_list:
                    self.map[str(root_com)][str(paral_com)] = 1
                    # TODO: set invalid pairs to 0

    def increase_weight(self, root_com, paral_com):
        self.map[str(root_com)][str(paral_com)] += 1

    def disable_pairs(self, root_com, paral_com):
        self.map[str(root_com)][str(paral_com)] = 0

    def get_weight(self, root_com, paral_com):
        return self.map[str(root_com)][str(paral_com)]
    
    def set_weight(self, root_com, paral_com, weight):
        self.map[str(root_com)][str(paral_com)] = weight

    # select n parallel operations based on weights
    def select_parallel_ops(self, root_com, n):
        row_values = self.map[str(root_com)].values()
        weights = list(row_values)
        selected_coms = random.choices(self.temp_parallel_command_list, weights=weights, k=n)
        return selected_coms

    def update(self, root_op, parallel_ops):
        for para_op in parallel_ops:
            self.increase_weight(root_op,para_op)


# test commands map
# new_map = CommandMap()
# parallel_ops = new_map.select_parallel_ops(Command("hardlink", [Relation.SELF,Relation.SELF], Ano.NORMAL),3)
# for op in parallel_ops:
#     print("<",op.opcode,",",op.relation,",",op.ano,">")
    
#     < rename , [<Relation.FATHER: 'father'>, <Relation.NREL: 'nrel'>] , Ano.NORMAL >
#     < truncate-overwrite , [<Relation.SELF: 'self'>] , Ano.DELAY >
#     < symlink , [<Relation.SELF: 'self'>, <Relation.SON: 'son'>] , Ano.SUSPEND >